package main

import (
	"fmt"
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
	sha      string
	err      error
}

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
	loading bool
	err     error

	file   int
	cursor int // index into the file's lines
	scroll int // first display row shown
	wrap   bool
	parsed map[int][]diffLine
	ask    *confirmation
	note   string
}

func newDiffView(r Repo, number int, title string) *diffView {
	return &diffView{id: nextViewID(), repo: r, number: number, title: title, wrap: true, parsed: map[int][]diffLine{},
		drafts: loadDrafts(r.Slug, number)}
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
			Head struct {
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
		return diffDataMsg{id: id, files: files, comments: comments, sha: clean(pr.Head.SHA), err: err}
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
		if msg.files != nil {
			changed := len(msg.files) != len(v.files) || msg.sha != v.sha
			v.files, v.sha = msg.files, msg.sha
			if changed {
				v.parsed = map[int][]diffLine{}
				v.file, v.cursor, v.scroll = min(v.file, max(0, len(v.files)-1)), 0, 0
			}
			v.threads = groupThreads(msg.comments)
		}
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
		return false, nil
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
	case "f":
		return true, pushView(v.filePicker())
	case "r":
		return true, v.fetch()
	case "o":
		return true, openURL(fmt.Sprintf("https://github.com/%s/pull/%d/files", v.repo.Slug, v.number))
	case "c":
		if v.cursor >= len(lines) {
			return true, nil
		}
		l := lines[v.cursor]
		side, n, ok := l.anchor()
		if !ok {
			v.note = "pick a line of code to comment on"
			return true, nil
		}
		path := v.path()
		c := newComposer(fmt.Sprintf("Comment on %s:%d", path, n), []string{codeLine(l, 0)}, "")
		c.done = func(body string) tea.Cmd {
			v.drafts = append(v.drafts, DraftComment{Path: path, Line: n, Side: side, Body: body})
			return v.saveDrafts(fmt.Sprintf("drafted · %d comment(s) wait for v to send the review", len(v.drafts)))
		}
		return true, pushView(c)
	case "e", "x":
		_, ds := v.threadsAt(v.cursor)
		if len(ds) == 0 {
			v.note = "no draft of yours on this line"
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
	v.file, v.cursor, v.scroll = i, 0, 0
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
	has := func(_ diffLine, i int) bool {
		ts, ds := v.threadsAt(i)
		return len(ts) > 0 || len(ds) > 0
	}
	if i := v.seek(dir, has); i != v.cursor {
		v.cursor = i
		return
	}
	for f := v.file + dir; f >= 0 && f < len(v.files); f += dir {
		if len(v.threads[v.files[f].Path]) == 0 && !v.hasDrafts(v.files[f].Path) {
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
		detail := sGood.Render(fmt.Sprintf("+%d", f.Additions)) + sBad.Render(fmt.Sprintf(" −%d", f.Deletions))
		if n := len(v.threads[f.Path]); n > 0 {
			detail += sMuted.Render(fmt.Sprintf(" · %d thread(s)", n))
		}
		if v.hasDrafts(f.Path) {
			detail += sWarn.Render(" · drafts")
		}
		if f.Previous != "" {
			detail += sMuted.Render(" · from " + f.Previous)
		}
		opts = append(opts, option{label: label, detail: detail, run: func() tea.Cmd {
			v.openFile(i)
			return nil
		}})
	}
	p := newPicker(fmt.Sprintf("Files in #%d", v.number), opts)
	p.sel = v.file
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

// row is one screen row of the diff, and the line it belongs to.
type row struct {
	text string
	line int
	code bool // a row of the line itself, rather than of a thread under it
}

const gutterW = 7 // "▸1234 +"

// codeLine draws a diff line's gutter and text on one row, cut at w when w > 0.
func codeLine(l diffLine, w int) string { return codeRows(l, w, false)[0] }

func codeRows(l diffLine, w int, wrap bool) []string {
	num := l.new
	if l.kind == '-' {
		num = l.old
	}
	text := strings.ReplaceAll(l.text, "\t", "    ")
	var style lipgloss.Style
	gutter := ""
	switch l.kind {
	case '@':
		return []string{sMuted.Render(" ⋯ ") + sCode.Render(truncate(text, max(4, w-4)))}
	case '\\':
		return []string{sMuted.Render("        " + text)}
	case '+':
		style, gutter = sGood, sMuted.Render(fmt.Sprintf("%4d ", num))+sGood.Render("+")
	case '-':
		style, gutter = sBad, sMuted.Render(fmt.Sprintf("%4d ", num))+sBad.Render("−")
	default:
		style, gutter = lipgloss.NewStyle(), sMuted.Render(fmt.Sprintf("%4d  ", num))
	}
	avail := w - gutterW
	if w <= 0 || avail < 10 {
		return []string{gutter + " " + style.Render(text)}
	}
	if !wrap {
		return []string{gutter + " " + style.Render(truncate(text, avail))}
	}
	r := []rune(text)
	var rows []string
	for first := true; first || len(r) > 0; first = false {
		n := min(avail, len(r))
		lead := gutter + " "
		if !first {
			lead = strings.Repeat(" ", gutterW)
		}
		rows = append(rows, lead+style.Render(string(r[:n])))
		r = r[n:]
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
	out := []string{lead + sWarn.Render("your draft · sent with the review (v) · e edit · x delete")}
	for _, l := range renderMarkdown(d.Body, w-10) {
		out = append(out, lead+"  "+l)
	}
	return out
}

func (v *diffView) rows(w int) []row {
	var out []row
	lines := v.lines()
	if od := v.outdated(); len(od) > 0 {
		out = append(out, row{text: sMuted.Render(fmt.Sprintf(" %d outdated thread(s), on code that has changed since:", len(od))), line: -1})
		for _, t := range od {
			out = append(out, row{text: "      " + sMuted.Render(fmt.Sprintf("line %d", t.root.Original)), line: -1})
			for _, r := range threadRows(t, w) {
				out = append(out, row{text: r, line: -1})
			}
		}
		out = append(out, row{line: -1})
	}
	for i, l := range lines {
		for _, r := range codeRows(l, w, v.wrap) {
			out = append(out, row{text: r, line: i, code: true})
		}
		ts, ds := v.threadsAt(i)
		for _, t := range ts {
			for _, r := range threadRows(t, w) {
				out = append(out, row{text: r, line: i})
			}
		}
		for _, j := range ds {
			for _, r := range draftRows(v.drafts[j], w) {
				out = append(out, row{text: r, line: i})
			}
		}
	}
	return out
}

func (v *diffView) View(w, h int) string {
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
		if n := len(v.drafts); n > 0 {
			line += sWarn.Render(fmt.Sprintf("  %d draft(s)", n))
		}
		head = append(head, truncStyledLine(line, w))
	} else {
		head = append(head, "")
	}
	head = append(head, sRule.Render(strings.Repeat("─", w)))

	keys := [][2]string{{"↑↓", "line"}, {"n p", "file"}, {"f", "files"}, {"t", "next thread"}, {"c", "comment"},
		{"R", "reply"}, {"v", "send review"}, {"w", "wrap"}, {"esc", "back"}}
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
			if r.line == v.cursor {
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
			if r.code && r.line == v.cursor {
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
