package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// diffLine is one line of a file's patch, with its line numbers in the old and new file.
type diffLine struct {
	kind     byte // '@' a hunk header, ' ' context, '+' added, '-' removed, '\\' "no newline at end of file"
	text     string
	old, new int
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@(.*)$`)

func parsePatch(patch string) []diffLine {
	if patch == "" {
		return nil
	}
	var out []diffLine
	old, new := 0, 0
	for _, l := range strings.Split(patch, "\n") {
		if m := hunkHeader.FindStringSubmatch(l); m != nil {
			old, _ = strconv.Atoi(m[1])
			new, _ = strconv.Atoi(m[2])
			out = append(out, diffLine{kind: '@', text: strings.TrimSpace(m[3])})
			continue
		}
		if l == "" {
			continue
		}
		switch l[0] {
		case '+':
			out = append(out, diffLine{kind: '+', text: l[1:], new: new})
			new++
		case '-':
			out = append(out, diffLine{kind: '-', text: l[1:], old: old})
			old++
		case '\\':
			out = append(out, diffLine{kind: '\\', text: strings.TrimSpace(l[1:])})
		default:
			out = append(out, diffLine{kind: ' ', text: l[1:], old: old, new: new})
			old++
			new++
		}
	}
	return out
}

// anchor is where a line comment attaches: the side of the diff and the line number on it, as GitHub counts them.
func (l diffLine) anchor() (side string, line int, ok bool) {
	switch l.kind {
	case '+', ' ':
		return "RIGHT", l.new, true
	case '-':
		return "LEFT", l.old, true
	}
	return "", 0, false
}

func anchorKey(side string, line int) string { return side + ":" + strconv.Itoa(line) }

// thread is a review comment and its replies, as the diff shows them.
type thread struct {
	root    ReviewComment
	replies []ReviewComment
}

type diffDataMsg struct {
	id       int
	files    []File
	comments []ReviewComment
	state    *ReviewState
	sha      string
	title    string
	err      error
}

// sinceMsg brings what changed since your last review, for S.
type sinceMsg struct {
	id    int
	files []File
	err   error
}

func (m sinceMsg) viewID() int { return m.id }

func (m diffDataMsg) viewID() int { return m.id }

// diffView is a pull request's changes, a file at a time, with its review threads under the lines they are on. c
// drafts a comment on the line under the cursor; v sends the drafts as one review.
type diffView struct {
	id      int
	repo    Repo
	number  int
	title   string
	sha     string
	files   []File
	threads map[string][]thread // by path, then found by anchor
	drafts  []DraftComment
	state   *ReviewState // thread ids and resolution, files viewed, your last review; nil until loaded
	loading bool
	err     error

	// since shows only what changed since your last review (S); all keeps the whole pull request meanwhile.
	since        bool
	sinceLoading bool
	all          []File

	file   int
	cursor int // index into the file's lines
	scroll int // first display row shown
	wrap   bool
	split  bool // old and new side by side, in a wide pane (s)
	parsed map[int][]diffLine
	hl     map[string]*highlighter // by path
	ask    *confirmation
	note   string
	width  int // the last width drawn at
	mark   int // where a comment on several lines starts (m), or -1
}

func newDiffView(r Repo, number int, title string) *diffView {
	return &diffView{id: nextViewID(), repo: r, number: number, title: title, wrap: true, parsed: map[int][]diffLine{},
		hl: map[string]*highlighter{}, drafts: loadDrafts(r.Slug, number), mark: -1}
}

func (v *diffView) ID() int          { return v.id }
func (v *diffView) Close()           {}
func (v *diffView) Init() tea.Cmd    { return v.fetch() }
func (v *diffView) Refresh() tea.Cmd { return v.fetch() }

func (v *diffView) fetch() tea.Cmd {
	v.loading = true
	id, repo, n := v.id, v.repo.Slug, v.number
	return func() tea.Msg {
		var pr struct {
			Title string `json:"title"`
			Head  struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if err := ghGet(fmt.Sprintf("repos/%s/pulls/%d", repo, n), &pr); err != nil {
			return diffDataMsg{id: id, err: err}
		}
		files, err := fetchFiles(repo, n)
		if err != nil {
			return diffDataMsg{id: id, err: err}
		}
		comments, err := fetchReviewComments(repo, n)
		if err != nil {
			return diffDataMsg{id: id, err: err}
		}
		state, err := fetchReviewState(repo, n)
		return diffDataMsg{id: id, files: files, comments: comments, state: state, sha: clean(pr.Head.SHA), title: clean(pr.Title), err: err}
	}
}

func (v *diffView) lines() []diffLine {
	if v.file >= len(v.files) {
		return nil
	}
	if l, ok := v.parsed[v.file]; ok {
		return l
	}
	l := parsePatch(v.files[v.file].Patch)
	v.parsed[v.file] = l
	return l
}

func (v *diffView) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case diffDataMsg:
		v.loading = false
		if msg.err != nil {
			v.err = msg.err
		} else {
			v.err = nil
		}
		if msg.state != nil {
			v.state = msg.state
		}
		if msg.title != "" {
			v.title = msg.title
		}
		if msg.files != nil {
			if v.since && msg.sha != v.sha { // new commits: what changed since the review is out of date
				v.since, v.all = false, nil
				v.note = "new commits since you opened this · showing everything; S compares with your review again"
			}
			if v.since { // keep showing what changed since the review; the whole is for when S goes back
				v.all = msg.files
			} else {
				changed := len(msg.files) != len(v.files) || msg.sha != v.sha
				v.files, v.sha = msg.files, msg.sha
				if changed {
					v.parsed = map[int][]diffLine{}
					v.file, v.cursor, v.scroll = min(v.file, max(0, len(v.files)-1)), 0, 0
				}
			}
			v.threads = groupThreads(msg.comments)
		}
	case sinceMsg:
		v.loading, v.sinceLoading = false, false
		if msg.err != nil {
			v.note = "can't compare with your last review: " + errText(msg.err)
			return true, nil
		}
		if !v.since {
			v.all = v.files
		}
		v.files, v.since = msg.files, true
		v.parsed, v.file, v.cursor, v.scroll = map[int][]diffLine{}, 0, 0, 0
		v.note = fmt.Sprintf("%d file(s) changed since your last review · S shows everything", len(v.files))
	case draftDeleteMsg:
		if msg.index < len(v.drafts) {
			v.drafts = append(v.drafts[:msg.index:msg.index], v.drafts[msg.index+1:]...)
		}
		return true, v.saveDrafts("draft deleted")
	case tea.KeyMsg:
		return v.key(msg)
	}
	return true, nil
}

// groupThreads gathers replies under the comment they answer, by file.
func groupThreads(cs []ReviewComment) map[string][]thread {
	byID := map[int64]*thread{}
	var roots []*thread
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Created.Before(cs[j].Created) })
	for _, c := range cs {
		if c.ReplyTo == 0 {
			t := &thread{root: c}
			byID[c.ID] = t
			roots = append(roots, t)
		}
	}
	for _, c := range cs {
		if c.ReplyTo != 0 {
			if t := byID[c.ReplyTo]; t != nil {
				t.replies = append(t.replies, c)
			}
		}
	}
	out := map[string][]thread{}
	for _, t := range roots {
		out[t.root.Path] = append(out[t.root.Path], *t)
	}
	return out
}

func (v *diffView) path() string {
	if v.file < len(v.files) {
		return v.files[v.file].Path
	}
	return ""
}

// threadsAt returns the threads and drafts on the line at index i of the current file.
func (v *diffView) threadsAt(i int) ([]thread, []int) {
	lines := v.lines()
	if i >= len(lines) {
		return nil, nil
	}
	side, n, ok := lines[i].anchor()
	if !ok {
		return nil, nil
	}
	var ts []thread
	for _, t := range v.threads[v.path()] {
		if !t.root.Outdated && t.root.Line == n && or(t.root.Side, "RIGHT") == side {
			ts = append(ts, t)
		}
	}
	var ds []int
	for j, d := range v.drafts {
		if d.Path == v.path() && d.Line == n && d.Side == side {
			ds = append(ds, j)
		}
	}
	return ts, ds
}

// resolved says whether a thread is resolved, as far as the review state knows.
func (v *diffView) resolved(t thread) bool {
	return v.state != nil && v.state.Threads[t.root.ID].Resolved
}

// viewed says whether you ticked "Viewed" on a file.
func (v *diffView) viewed(path string) bool { return v.state != nil && v.state.Viewed[path] }

func (v *diffView) outdated() []thread {
	var ts []thread
	for _, t := range v.threads[v.path()] {
		if t.root.Outdated {
			ts = append(ts, t)
		}
	}
	return ts
}

func (v *diffView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	if v.ask != nil {
		done, ok := v.ask.key(msg)
		if done {
			cmd := v.ask.cmd
			v.ask = nil
			if ok {
				return true, cmd
			}
		}
		return true, nil
	}
	v.note = ""
	lines := v.lines()
	last := max(0, len(lines)-1)
	switch msg.String() {
	case "esc", "q":
		if v.mark >= 0 && msg.String() == "esc" {
			v.mark = -1
			v.note = "range cleared"
			return true, nil
		}
		return false, nil
	case "m":
		if v.cursor < len(lines) {
			if _, n, ok := lines[v.cursor].anchor(); ok {
				v.mark = v.cursor
				v.note = fmt.Sprintf("from line %d · move to the last line, then c comments or C suggests · esc clears", n)
				return true, nil
			}
		}
		v.note = "mark a line of code"
	case "down", "j":
		v.cursor = min(last, v.cursor+1)
	case "up", "k":
		v.cursor = max(0, v.cursor-1)
	case "pgdown", "ctrl+d", " ":
		v.cursor = min(last, v.cursor+15)
	case "pgup", "ctrl+u":
		v.cursor = max(0, v.cursor-15)
	case "g", "home":
		v.cursor = 0
	case "G", "end":
		v.cursor = last
	case "n", "]", "right", "l":
		v.openFile(v.file + 1)
	case "p", "[", "left", "h":
		v.openFile(v.file - 1)
	case "}":
		v.cursor = v.seek(1, func(l diffLine, _ int) bool { return l.kind == '@' })
	case "{":
		v.cursor = v.seek(-1, func(l diffLine, _ int) bool { return l.kind == '@' })
	case "t":
		v.seekThread(1)
	case "T":
		v.seekThread(-1)
	case "w":
		v.wrap = !v.wrap
	case "s":
		v.split = !v.split
		if v.split && v.width > 0 && v.width < splitMin {
			v.note = fmt.Sprintf("side by side needs a pane %d columns wide; this one is %d", splitMin, v.width)
		}
	case "f":
		return true, pushView(v.filePicker())
	case "r":
		sharedRefresh()
		return true, v.fetch()
	case "o":
		return true, openURL(fmt.Sprintf("https://github.com/%s/pull/%d/files", v.repo.Slug, v.number))
	case "z":
		return true, v.toggleResolved()
	case "V":
		return true, v.toggleViewed()
	case "S":
		return true, v.toggleSince()
	case "a":
		return true, v.applySuggestion()
	case "c", "C":
		if v.cursor >= len(lines) {
			return true, nil
		}
		l := lines[v.cursor]
		side, n, ok := l.anchor()
		if !ok {
			v.note = "pick a line of code to comment on"
			return true, nil
		}
		// From the mark, when there is one: a comment on every line between.
		lo, hi := v.cursor, v.cursor
		startLine, startSide := 0, ""
		if v.mark >= 0 && v.mark != v.cursor {
			lo, hi = min(v.mark, v.cursor), max(v.mark, v.cursor)
			for i := lo; i <= hi; i++ {
				if lines[i].kind == '@' {
					v.note = "a comment on several lines stays within one hunk · m marks a new start"
					return true, nil
				}
			}
			s1, n1, ok1 := lines[lo].anchor()
			s2, n2, ok2 := lines[hi].anchor()
			if !ok1 || !ok2 || s1 != s2 {
				v.note = "start and end on the same side: both in the new code, or both removed lines"
				return true, nil
			}
			l, side, n = lines[hi], s2, n2
			startLine, startSide = n1, s1
		}
		initial := ""
		if msg.String() == "C" { // suggest a change: the lines as they are, to edit into what they should be
			if side != "RIGHT" {
				v.note = "a suggestion replaces lines of the new file · pick added or unchanged lines"
				return true, nil
			}
			var text []string
			for i := lo; i <= hi; i++ {
				if lines[i].kind == '+' || lines[i].kind == ' ' {
					text = append(text, lines[i].text)
				}
			}
			initial = "```suggestion\n" + strings.Join(text, "\n") + "\n```"
		}
		if v.since && side == "LEFT" {
			v.note = "a removed line here is from your last review, not the base · S shows the whole diff to comment on it"
			return true, nil
		}
		path := v.path()
		where := fmt.Sprintf("%s:%d", path, n)
		var context []string
		for i := lo; i <= hi && len(context) < 8; i++ {
			context = append(context, codeLine(lines[i], 0))
		}
		if startLine != 0 {
			where = fmt.Sprintf("%s:%d-%d", path, startLine, n)
		}
		c := newComposer("Comment on "+where, context, initial)
		c.done = func(body string) tea.Cmd {
			v.mark = -1
			v.drafts = append(v.drafts, DraftComment{Path: path, Line: n, Side: side, Body: body, StartLine: startLine, StartSide: startSide})
			return v.saveDrafts(fmt.Sprintf("drafted · %d comment(s) wait for v to send the review", len(v.drafts)))
		}
		return true, pushView(c)
	case "e", "x":
		ts, ds := v.threadsAt(v.cursor)
		if len(ds) == 0 {
			// No draft here: your comment already posted on this line, if there is one.
			if c := v.yourComment(ts); c != nil {
				return true, v.changePosted(*c, msg.String() == "x")
			}
			v.note = "no draft or comment of yours on this line"
			return true, nil
		}
		j := ds[len(ds)-1]
		if msg.String() == "x" {
			id := v.id
			v.ask = newConfirmation("Delete this draft comment?", func() tea.Msg { return draftDeleteMsg{id: id, index: j} })
			return true, nil
		}
		d := v.drafts[j]
		c := newComposer(fmt.Sprintf("Edit draft on %s:%d", d.Path, d.Line), []string{codeLine(lines[v.cursor], 0)}, d.Body)
		c.done = func(body string) tea.Cmd {
			if j < len(v.drafts) {
				v.drafts[j].Body = body
			}
			return v.saveDrafts("draft updated")
		}
		return true, pushView(c)
	case "R":
		ts, _ := v.threadsAt(v.cursor)
		if len(ts) == 0 {
			v.note = "no thread on this line to reply to · c starts one"
			return true, nil
		}
		t := ts[len(ts)-1]
		first, _, _ := strings.Cut(t.root.Body, "\n")
		c := newComposer(fmt.Sprintf("Reply to %s on %s:%d", t.root.User.Login, t.root.Path, t.root.Line),
			[]string{sMuted.Render(truncate(first, 200))}, "")
		repo, n, id := v.repo.Slug, v.number, t.root.ID
		c.send = func(body string) error { return replyToComment(repo, n, id, body) }
		c.done = func(string) tea.Cmd {
			return tea.Batch(func() tea.Msg { return actionMsg{text: "reply posted"} }, v.fetch())
		}
		return true, pushView(c)
	case "v":
		if v.sha == "" {
			return true, nil
		}
		return true, pushView(reviewPicker(v.repo.Slug, v.number, v.sha, append([]DraftComment(nil), v.drafts...), func(text string) tea.Cmd {
			v.drafts = nil
			return tea.Batch(func() tea.Msg { return actionMsg{text: text, refresh: true} }, v.fetch())
		}))
	}
	return true, nil
}

// inRange says a row's line is between the mark and the cursor.
func (v *diffView) inRange(r row) bool {
	lo, hi := min(v.mark, v.cursor), max(v.mark, v.cursor)
	return r.line >= lo && r.line <= hi || r.alt >= lo && r.alt <= hi && r.alt >= 0
}

// yourComment finds your latest posted comment among a line's threads.
func (v *diffView) yourComment(ts []thread) *ReviewComment {
	if v.state == nil || v.state.Viewer == "" {
		return nil
	}
	var found *ReviewComment
	for _, t := range ts {
		for _, c := range append([]ReviewComment{t.root}, t.replies...) {
			if c.User.Login == v.state.Viewer && (found == nil || c.Created.After(found.Created)) {
				c := c
				found = &c
			}
		}
	}
	return found
}

// changePosted edits, or after asking deletes, a comment of yours already on GitHub.
func (v *diffView) changePosted(c ReviewComment, del bool) tea.Cmd {
	repo := v.repo.Slug
	if del {
		v.ask = newConfirmation("Delete your posted comment?", func() tea.Msg {
			if err := deleteReviewComment(repo, c.ID); err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{text: "comment deleted", refresh: true}
		})
		return nil
	}
	comp := newComposer(fmt.Sprintf("Edit your comment on %s:%d", c.Path, c.Line), nil, c.Body)
	comp.send = func(body string) error { return editReviewComment(repo, c.ID, body) }
	comp.done = func(string) tea.Cmd {
		return tea.Batch(func() tea.Msg { return actionMsg{text: "comment updated"} }, v.fetch())
	}
	return pushView(comp)
}

// suggestionIn reads the lines a comment suggests, from its first suggestion block.
func suggestionIn(body string) ([]string, bool) {
	var out []string
	in := false
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case !in && strings.HasPrefix(t, "```") && strings.TrimSpace(t[3:]) == "suggestion":
			in = true
		case in && strings.HasPrefix(t, "```"):
			return out, true
		case in:
			out = append(out, line)
		}
	}
	return nil, false
}

// applySuggestion writes the suggestion on the cursor's line into your checkout, after asking, when the checkout is
// at the pull request's head and the lines still read as the diff shows them. It does not commit: that is yours.
func (v *diffView) applySuggestion() tea.Cmd {
	ts, _ := v.threadsAt(v.cursor)
	var c *ReviewComment
	for _, t := range ts {
		// A suggestion replaces lines of the new file, so only a thread on that side has one to apply: the line
		// numbers of a comment on a removed line are the base file's.
		if or(t.root.Side, "RIGHT") != "RIGHT" || or(t.root.StartSide, "RIGHT") != "RIGHT" {
			continue
		}
		for _, rc := range append([]ReviewComment{t.root}, t.replies...) {
			if _, ok := suggestionIn(rc.Body); ok {
				rc := rc
				c = &rc
			}
		}
	}
	if c == nil {
		v.note = "no suggested change to the new code on this line"
		return nil
	}
	root := v.repo.Root
	if root == "" {
		v.note = "applying needs a checkout of this repository"
		return nil
	}
	if out, err := runIn(root, "git", "rev-parse", "HEAD"); err != nil || strings.TrimSpace(string(out)) != v.sha {
		v.note = "your checkout is not at this pull request's head · check it out first (C on the list or the pull request)"
		return nil
	}
	path := v.path()
	if _, err := runIn(root, "git", "diff", "--quiet", "HEAD", "--", path); err != nil {
		v.note = path + " has changes you have not committed · commit or stash them first, so the lines match"
		return nil
	}
	start, end := c.StartLine, v.threadLine(*c, ts)
	if start == 0 {
		start = end
	}
	// The lines as the diff shows them, which the file must still read as.
	shown := map[int]string{}
	for _, l := range v.lines() {
		if l.kind == '+' || l.kind == ' ' {
			shown[l.new] = l.text
		}
	}
	var want []string
	for n := start; n <= end; n++ {
		t, ok := shown[n]
		if !ok {
			v.note = fmt.Sprintf("line %d is outside the diff, so ghdeck can't check it · apply this one on GitHub", n)
			return nil
		}
		want = append(want, t)
	}
	v.ask = newConfirmation(fmt.Sprintf("Apply %s's suggestion to %s:%d-%d in your checkout?", c.User.Login, path, start, end),
		func() tea.Msg {
			if err := applySuggestion(root, path, start, end, want, c.Body); err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{text: fmt.Sprintf("applied to %s · commit and push it to send the change", path)}
		})
	return nil
}

// threadLine is where a comment's thread sits: a reply carries no line of its own, so it takes its thread's.
func (v *diffView) threadLine(c ReviewComment, ts []thread) int {
	if c.Line != 0 {
		return c.Line
	}
	for _, t := range ts {
		if t.root.ID == c.ReplyTo {
			return t.root.Line
		}
	}
	return 0
}

// applySuggestion replaces lines start through end of a file in the checkout with a comment's suggestion, when they
// still read as want. The file is the pull request's, so its name is untrusted: it must be a plain file whose real
// path, symlinks followed, is inside the checkout.
func applySuggestion(root, path string, start, end int, want []string, body string) error {
	repl, ok := suggestionIn(body)
	if !ok {
		return fmt.Errorf("no suggestion in the comment")
	}
	full := filepath.Join(root, filepath.FromSlash(path))
	if rel, err := filepath.Rel(root, full); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is outside the checkout", path)
	}
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a plain file (a symlink, say), so ghdeck won't write it", path)
	}
	realRoot, err1 := filepath.EvalSymlinks(root)
	realFull, err2 := filepath.EvalSymlinks(full)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("can't resolve %s in the checkout", path)
	}
	if rel, err := filepath.Rel(realRoot, realFull); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s leads outside the checkout", path)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	text := string(b)
	trailing := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if start < 1 || end < start || end > len(lines) {
		return fmt.Errorf("%s has %d lines; the suggestion is for %d-%d", path, len(lines), start, end)
	}
	for i, w := range want {
		if strings.TrimRight(lines[start-1+i], "\r") != w {
			return fmt.Errorf("%s:%d no longer reads as the diff shows it; nothing was changed", path, start+i)
		}
	}
	out := append(append(append([]string{}, lines[:start-1]...), repl...), lines[end:]...)
	text = strings.Join(out, "\n")
	if trailing {
		text += "\n"
	}
	return os.WriteFile(full, []byte(text), info.Mode().Perm())
}

// toggleResolved resolves the thread on the cursor's line, or opens it again.
func (v *diffView) toggleResolved() tea.Cmd {
	ts, _ := v.threadsAt(v.cursor)
	if len(ts) == 0 {
		v.note = "no thread on this line · t finds the next"
		return nil
	}
	t := ts[len(ts)-1]
	for _, c := range ts { // an open one first
		if !v.resolved(c) {
			t = c
			break
		}
	}
	var ts2 ThreadState
	ok := false
	if v.state != nil {
		ts2, ok = v.state.Threads[t.root.ID]
	}
	if !ok {
		v.note = "GitHub has not said which thread this is yet · r reloads"
		return nil
	}
	resolve := !ts2.Resolved
	ts2.Resolved = resolve
	v.state.Threads[t.root.ID] = ts2 // shown at once; the reload after confirms it
	text := "thread resolved"
	if !resolve {
		text = "thread opened again"
	}
	return func() tea.Msg {
		if err := setResolved(ts2.ID, resolve); err != nil {
			return actionMsg{err: err, refresh: true}
		}
		return actionMsg{text: text, refresh: true}
	}
}

// toggleViewed ticks the file's "Viewed" box on GitHub, as its own page does, and moves on to the next file not
// viewed yet.
func (v *diffView) toggleViewed() tea.Cmd {
	path := v.path()
	if path == "" || v.state == nil || v.state.PRID == "" {
		return nil
	}
	viewed := !v.state.Viewed[path]
	v.state.Viewed[path] = viewed
	prID := v.state.PRID
	if viewed {
		for i := 1; i < len(v.files); i++ {
			if f := (v.file + i) % len(v.files); !v.viewed(v.files[f].Path) {
				v.openFile(f)
				break
			}
		}
	}
	n := 0
	for _, f := range v.files {
		if v.viewed(f.Path) {
			n++
		}
	}
	text := fmt.Sprintf("viewed · %d of %d files", n, len(v.files))
	if !viewed {
		text = "not viewed · " + path
	}
	return func() tea.Msg {
		if err := setViewed(prID, path, viewed); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: text}
	}
}

// toggleSince shows only what changed since your last review, or the whole pull request again.
func (v *diffView) toggleSince() tea.Cmd {
	if v.since {
		v.files, v.all, v.since = v.all, nil, false
		v.parsed, v.file, v.cursor, v.scroll = map[int][]diffLine{}, 0, 0, 0
		v.note = "the whole pull request"
		return nil
	}
	switch {
	case v.state == nil:
		return nil
	case v.state.LastReview == "":
		v.note = "you have not reviewed this pull request yet"
		return nil
	case v.state.LastReview == v.sha:
		v.note = "nothing new since your last review"
		return nil
	}
	if v.sinceLoading {
		return nil
	}
	v.loading, v.sinceLoading = true, true
	id, repo, from, to := v.id, v.repo.Slug, v.state.LastReview, v.sha
	return func() tea.Msg {
		files, err := fetchFilesSince(repo, from, to)
		return sinceMsg{id: id, files: files, err: err}
	}
}

type draftDeleteMsg struct {
	id    int
	index int
}

func (m draftDeleteMsg) viewID() int { return m.id }

// saveDrafts writes the drafts at once, on the UI, so saves land in order and a review sent next has them all.
func (v *diffView) saveDrafts(text string) tea.Cmd {
	err := saveDrafts(v.repo.Slug, v.number, v.drafts)
	return func() tea.Msg {
		if err != nil {
			return actionMsg{err: fmt.Errorf("saving drafts: %w", err)}
		}
		return actionMsg{text: text}
	}
}

func (v *diffView) openFile(i int) {
	if i < 0 || i >= len(v.files) || i == v.file {
		return
	}
	v.file, v.cursor, v.scroll, v.mark = i, 0, 0, -1
}

// seek finds the next line, forwards or backwards from the cursor, that ok accepts, or stays put.
func (v *diffView) seek(dir int, ok func(diffLine, int) bool) int {
	lines := v.lines()
	for i := v.cursor + dir; i >= 0 && i < len(lines); i += dir {
		if ok(lines[i], i) {
			return i
		}
	}
	return v.cursor
}

// seekThread moves to the next line with a thread or a draft, on to the next file when this one has no more.
func (v *diffView) seekThread(dir int) {
	// Resolved threads are done with; t skips them.
	has := func(_ diffLine, i int) bool {
		ts, ds := v.threadsAt(i)
		for _, t := range ts {
			if !v.resolved(t) {
				return true
			}
		}
		return len(ds) > 0
	}
	if i := v.seek(dir, has); i != v.cursor {
		v.cursor = i
		return
	}
	for f := v.file + dir; f >= 0 && f < len(v.files); f += dir {
		if v.openThreads(v.files[f].Path) == 0 && !v.hasDrafts(v.files[f].Path) {
			continue
		}
		v.openFile(f)
		v.cursor = -1
		if dir < 0 {
			v.cursor = len(v.lines())
		}
		if i := v.seek(dir, has); i >= 0 && i < len(v.lines()) {
			v.cursor = i
		} else {
			v.cursor = 0
		}
		return
	}
	v.note = "no more threads"
}

// openThreads counts a file's unresolved threads.
func (v *diffView) openThreads(path string) int {
	n := 0
	for _, t := range v.threads[path] {
		if !v.resolved(t) {
			n++
		}
	}
	return n
}

func (v *diffView) hasDrafts(path string) bool {
	for _, d := range v.drafts {
		if d.Path == path {
			return true
		}
	}
	return false
}

func (v *diffView) filePicker() *picker {
	var opts []option
	for i, f := range v.files {
		i := i
		label := fileStatusIcon(f.Status) + " " + f.Path
		if v.viewed(f.Path) {
			label = sGood.Render("✓ ") + label
		} else {
			label = "  " + label
		}
		detail := sGood.Render(fmt.Sprintf("+%d", f.Additions)) + sBad.Render(fmt.Sprintf(" −%d", f.Deletions))
		if n := v.openThreads(f.Path); n > 0 {
			detail += sMuted.Render(fmt.Sprintf(" · %d open thread(s)", n))
		}
		if v.hasDrafts(f.Path) {
			detail += sWarn.Render(" · drafts")
		}
		if f.Previous != "" {
			detail += sMuted.Render(" · from " + f.Previous)
		}
		opts = append(opts, option{label: label, match: f.Path, detail: detail, run: func() tea.Cmd {
			v.openFile(i)
			return nil
		}})
	}
	p := newPicker(fmt.Sprintf("Files in #%d", v.number), opts)
	p.sel = v.file
	p.filterable = true
	return p
}

func fileStatusIcon(status string) string {
	switch status {
	case "added":
		return sGood.Render("A")
	case "removed":
		return sBad.Render("D")
	case "renamed":
		return sWarn.Render("R")
	}
	return sMuted.Render("M")
}

// ---- drawing ----

// row is one screen row of the diff, and the line it belongs to: two, side by side, where a removed line faces the
// line that replaced it.
type row struct {
	text string
	line int
	alt  int  // the other side's line, side by side; -1 when none
	code bool // a row of the line itself, rather than of a thread under it
}

func (r row) has(i int) bool { return r.line == i || r.alt == i && r.alt >= 0 }

// highlighter is the current file's, made on first use.
func (v *diffView) highlighter() *highlighter {
	p := v.path()
	h, ok := v.hl[p]
	if !ok {
		h = newHighlighter(p)
		v.hl[p] = h
	}
	return h
}

// splitMin is the narrowest pane that shows the diff side by side: two halves wide enough to read.
const splitMin = 120

const gutterW = 7 // "▸1234 +"

// codeLine draws a diff line's gutter and text on one row, cut at w when w > 0.
func codeLine(l diffLine, w int) string { return codeRows(l, w, false, nil)[0] }

// codeRows draws a diff line, wrapped to w or cut there, its code coloured by hl when hl knows the language. Added
// and removed lines are tinted across the full width.
func codeRows(l diffLine, w int, wrap bool, hl *highlighter) []string {
	num := l.new
	if l.kind == '-' {
		num = l.old
	}
	text := strings.ReplaceAll(l.text, "\t", "    ")
	plain := hl == nil || hl.lexer == nil
	var bg, fg lipgloss.TerminalColor
	gutter := ""
	switch l.kind {
	case '@':
		return []string{sMuted.Render(" ⋯ ") + sCode.Render(truncate(text, max(4, w-4)))}
	case '\\':
		return []string{sMuted.Render("        " + text)}
	case '+':
		bg, gutter = cAddBg, sMuted.Render(fmt.Sprintf("%4d ", num))+sGood.Render("+")
		if plain {
			fg = cGood
		}
	case '-':
		bg, gutter = cDelBg, sMuted.Render(fmt.Sprintf("%4d ", num))+sBad.Render("−")
		if plain {
			fg = cBad
		}
	default:
		gutter = sMuted.Render(fmt.Sprintf("%4d  ", num))
	}
	runs := hl.runs(text)
	n := len([]rune(text))
	avail := w - gutterW
	fill := func(used int) string { // the tint carries on to the edge
		if bg == nil || avail-used <= 0 {
			return ""
		}
		return lipgloss.NewStyle().Background(bg).Render(strings.Repeat(" ", avail-used))
	}
	if w <= 0 || avail < 10 {
		return []string{gutter + " " + slice(runs, 0, n, bg, fg)}
	}
	if !wrap {
		if n > avail {
			return []string{gutter + " " + slice(runs, 0, avail-1, bg, fg) + sMuted.Render("…")}
		}
		return []string{gutter + " " + slice(runs, 0, n, bg, fg) + fill(n)}
	}
	var rows []string
	for at := 0; at == 0 || at < n; at += avail {
		lead := gutter + " "
		if at > 0 {
			lead = strings.Repeat(" ", gutterW)
		}
		end := min(n, at+avail)
		rows = append(rows, lead+slice(runs, at, end, bg, fg)+fill(end-at))
	}
	return rows
}

func threadRows(t thread, w int) []string {
	bar := sAccentBar
	var out []string
	add := func(c ReviewComment, reply bool) {
		lead := "      " + bar + " "
		who := sBold.Render(c.User.Login) + sMuted.Render(" · "+age(c.Created)+" ago")
		if reply {
			who = sMuted.Render("↳ ") + who
		}
		out = append(out, truncStyledLine(lead+who, w))
		for _, l := range renderMarkdown(c.Body, w-10) {
			out = append(out, lead+"  "+l)
		}
	}
	add(t.root, false)
	for _, r := range t.replies {
		add(r, true)
	}
	return out
}

var sAccentBar = lipgloss.NewStyle().Foreground(cAccent).Render("┃")

func draftRows(d DraftComment, w int) []string {
	lead := "      " + sWarn.Render("┃") + " "
	what := "your draft"
	if d.StartLine != 0 {
		what = fmt.Sprintf("your draft on lines %d-%d", d.StartLine, d.Line)
	}
	out := []string{lead + sWarn.Render(what+" · sent with the review (v) · e edit · x delete")}
	for _, l := range renderMarkdown(d.Body, w-10) {
		out = append(out, lead+"  "+l)
	}
	return out
}

func (v *diffView) rows(w int) []row {
	var out []row
	lines := v.lines()
	if od := v.outdated(); len(od) > 0 {
		out = append(out, row{text: sMuted.Render(fmt.Sprintf(" %d outdated thread(s), on code that has changed since:", len(od))), line: -1, alt: -1})
		for _, t := range od {
			out = append(out, row{text: "      " + sMuted.Render(fmt.Sprintf("line %d", t.root.Original)), line: -1, alt: -1})
			for _, r := range threadRows(t, w) {
				out = append(out, row{text: r, line: -1, alt: -1})
			}
		}
		out = append(out, row{line: -1, alt: -1})
	}
	if v.split && w >= splitMin {
		return append(out, v.splitRows(w)...)
	}
	hl := v.highlighter()
	for i, l := range lines {
		for _, r := range codeRows(l, w, v.wrap, hl) {
			out = append(out, row{text: r, line: i, alt: -1, code: true})
		}
		out = append(out, v.notesRows(i, w)...)
	}
	return out
}

// notesRows draws the threads and drafts under line i.
func (v *diffView) notesRows(i, w int) []row {
	var out []row
	{
		ts, ds := v.threadsAt(i)
		for _, t := range ts {
			if v.resolved(t) { // folded to a line: done with, but still there
				first, _, _ := strings.Cut(t.root.Body, "\n")
				text := fmt.Sprintf("      %s %s · %s", sGood.Render("✓ resolved"), sMuted.Render(t.root.User.Login+": "+first),
					sMuted.Render(plural(1+len(t.replies), "comment")+" · z opens it"))
				out = append(out, row{text: truncStyledLine(text, w), line: i, alt: -1})
				continue
			}
			for _, r := range threadRows(t, w) {
				out = append(out, row{text: r, line: i, alt: -1})
			}
		}
		for _, j := range ds {
			for _, r := range draftRows(v.drafts[j], w) {
				out = append(out, row{text: r, line: i, alt: -1})
			}
		}
	}
	return out
}

// splitRows lays the diff out side by side: the old file on the left, the new on the right. Within a change, the
// removed lines face the added ones that replaced them; context lines face themselves.
func (v *diffView) splitRows(w int) []row {
	lines, hl := v.lines(), v.highlighter()
	half := (w - 1) / 2
	sep := sRule.Render("│")
	var out []row
	cell := func(i int, old bool) []string {
		if i < 0 {
			return []string{""}
		}
		l := lines[i]
		if old { // a context line on the left goes by its number in the old file
			l.new = l.old
		}
		return codeRows(l, half, v.wrap, hl)
	}
	pair := func(left, right int) {
		l, r := cell(left, true), cell(right, false)
		for k := 0; k < max(len(l), len(r)); k++ {
			a, b := "", ""
			if k < len(l) {
				a = l[k]
			}
			if k < len(r) {
				b = r[k]
			}
			line, alt := left, right
			if line < 0 {
				line, alt = right, -1
			}
			out = append(out, row{text: pad(truncStyledLine(a, half), half) + sep + truncStyledLine(b, half), line: line, alt: alt, code: true})
		}
		if left >= 0 {
			out = append(out, v.notesRows(left, w)...)
		}
		if right >= 0 && right != left {
			out = append(out, v.notesRows(right, w)...)
		}
	}
	for i := 0; i < len(lines); {
		switch lines[i].kind {
		case '-', '+':
			var dels, adds []int
			for ; i < len(lines) && lines[i].kind == '-'; i++ {
				dels = append(dels, i)
			}
			for ; i < len(lines) && lines[i].kind == '+'; i++ {
				adds = append(adds, i)
			}
			for k := 0; k < max(len(dels), len(adds)); k++ {
				left, right := -1, -1
				if k < len(dels) {
					left = dels[k]
				}
				if k < len(adds) {
					right = adds[k]
				}
				pair(left, right)
			}
		case ' ':
			pair(i, i)
			i++
		default: // a hunk header, or "no newline at end of file", across both
			for _, r := range codeRows(lines[i], w, false, hl) {
				out = append(out, row{text: r, line: i, alt: -1, code: true})
			}
			i++
		}
	}
	return out
}

func (v *diffView) View(w, h int) string {
	v.width = w
	var head []string
	title := fmt.Sprintf("#%d %s", v.number, v.title)
	spin := ""
	if v.loading {
		spin = sMuted.Render(" ⟳")
	}
	head = append(head, truncStyledLine(sTitle.Render(title)+spin, w))
	if v.file < len(v.files) {
		f := v.files[v.file]
		where := fmt.Sprintf(" file %d/%d ", v.file+1, len(v.files))
		stats := sGood.Render(fmt.Sprintf("+%d", f.Additions)) + sBad.Render(fmt.Sprintf(" −%d", f.Deletions))
		line := sMuted.Render(where) + fileStatusIcon(f.Status) + " " + sBold.Render(f.Path) + "  " + stats
		if v.viewed(f.Path) {
			line += sGood.Render("  ✓ viewed")
		}
		if v.since {
			line += sWarn.Render("  since your last review")
		}
		if n := len(v.drafts); n > 0 {
			line += sWarn.Render(fmt.Sprintf("  %d draft(s)", n))
		}
		head = append(head, truncStyledLine(line, w))
	} else {
		head = append(head, "")
	}
	head = append(head, sRule.Render(strings.Repeat("─", w)))

	keys := [][2]string{{"↑↓", "line"}, {"n p", "file"}, {"f", "files"}, {"V", "viewed"}, {"t", "next thread"},
		{"c", "comment"}, {"m", "mark range"}, {"C", "suggest"}, {"a", "apply suggestion"}, {"R", "reply"}, {"z", "resolve"}, {"S", "since review"}, {"v", "send review"}, {"s", "side by side"}, {"w", "wrap"},
		{"esc", "back"}}
	help := keyHelp(keys, w)
	bodyH := max(1, h-len(head)-1-lipgloss.Height(help))
	var body []string
	switch {
	case v.files == nil && v.err == nil:
		body = []string{sMuted.Render(" loading…")}
	case len(v.files) == 0 && v.err == nil:
		body = []string{sMuted.Render(" no changes")}
	case v.file < len(v.files) && v.files[v.file].Patch == "":
		body = []string{sMuted.Render(" No diff to show: the file is binary, or too large for GitHub to send. o opens it on GitHub.")}
	default:
		rows := v.rows(w)
		// Keep the cursor's line on screen, with room for the threads under it when they fit.
		first, lastRow := -1, -1
		for i, r := range rows {
			if r.has(v.cursor) {
				if first < 0 && r.code {
					first = i
				}
				lastRow = i
			}
		}
		if first >= 0 {
			if first < v.scroll {
				v.scroll = first
			}
			if lastRow >= v.scroll+bodyH {
				v.scroll = min(first, lastRow-bodyH+1)
			}
			if first == 0 || v.cursor == 0 {
				v.scroll = 0
			}
		}
		v.scroll = max(0, min(v.scroll, len(rows)-bodyH))
		for i := v.scroll; i < len(rows) && i < v.scroll+bodyH; i++ {
			r := rows[i]
			text := r.text
			if v.mark >= 0 && r.code && !r.has(v.cursor) && v.inRange(r) {
				text = lipgloss.NewStyle().Background(cSelBg).Render(pad(truncStyledLine(text, w), w))
			}
			if r.code && r.has(v.cursor) {
				text = sTitle.Render("▸") + strings.TrimPrefix(text, " ")
				text = lipgloss.NewStyle().Background(cSelBg).Render(pad(truncStyledLine(text, w), w))
			}
			body = append(body, text)
		}
	}
	for len(body) < bodyH {
		body = append(body, "")
	}
	status := ""
	switch {
	case v.ask != nil:
		status = v.ask.view()
	case v.err != nil:
		status = sBad.Render(" " + truncate(errText(v.err), w-2))
	case v.note != "":
		status = sWarn.Render(" " + v.note)
	}
	return strings.Join(head, "\n") + "\n" + strings.Join(body, "\n") + "\n" + status + "\n" + help
}
