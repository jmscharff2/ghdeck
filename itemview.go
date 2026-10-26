package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type itemDataMsg struct {
	id  int
	d   *Detail
	err error
}

func (m itemDataMsg) viewID() int { return m.id }

type itemTickMsg struct{ id, gen int }

func (m itemTickMsg) viewID() int { return m.id }

// itemView is one pull request or issue: its description, checks, reviews and conversation, and everything you can
// do to it.
type itemView struct {
	id      int
	repo    Repo
	item    Item // what the list knew, shown until the detail arrives
	d       *Detail
	err     error
	loading bool
	gen     int // which fetch's tick is current, so refreshes by hand do not pile up timers
	scroll  int
	ask     *confirmation
	lastW   int
	lines   []string
}

func newItemView(r Repo, it Item) *itemView {
	return &itemView{id: nextViewID(), repo: r, item: it}
}

func (v *itemView) ID() int          { return v.id }
func (v *itemView) Close()           {}
func (v *itemView) Init() tea.Cmd    { return v.fetch() }
func (v *itemView) Refresh() tea.Cmd { return v.fetch() }

func (v *itemView) fetch() tea.Cmd {
	v.loading = true
	id, repo, n := v.id, v.repo.Slug, v.item.Number
	return func() tea.Msg {
		d, err := fetchDetail(repo, n)
		return itemDataMsg{id: id, d: d, err: err}
	}
}

// pending says checks are still running, which is worth refreshing for sooner.
func (v *itemView) pending() bool {
	if v.d == nil {
		return false
	}
	for _, c := range v.d.Checks {
		if c.Status != "COMPLETED" {
			return true
		}
	}
	return false
}

func (v *itemView) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case itemDataMsg:
		v.loading = false
		if msg.err != nil {
			v.err = msg.err
		} else {
			v.d, v.err, v.item = msg.d, nil, msg.d.Item
			v.lines = nil
		}
		v.gen++
		every := time.Minute
		if v.pending() {
			every = 15 * time.Second
		}
		id, gen := v.id, v.gen
		return true, tea.Tick(pace(every), func(time.Time) tea.Msg { return itemTickMsg{id, gen} })
	case itemTickMsg:
		if msg.gen == v.gen {
			return true, v.fetch()
		}
	case tea.KeyMsg:
		return v.key(msg)
	}
	return true, nil
}

func (v *itemView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	if v.ask != nil {
		done, ok := v.ask.key(msg)
		if !done {
			return true, nil
		}
		cmd := v.ask.cmd
		v.ask = nil
		if ok {
			return true, cmd
		}
		return true, nil
	}
	it, repo := v.item, v.repo.Slug
	switch msg.String() {
	case "esc", "q":
		return false, nil
	case "up", "k":
		v.scroll = max(0, v.scroll-1)
	case "down", "j":
		v.scroll++
	case "pgup", "b", "ctrl+u":
		v.scroll = max(0, v.scroll-15)
	case "pgdown", " ", "ctrl+d":
		v.scroll += 15
	case "g", "home":
		v.scroll = 0
	case "G", "end":
		v.scroll = 1 << 30
	case "r":
		sharedRefresh()
		return true, v.fetch()
	case "o":
		return true, openURL(it.URL)
	case "c":
		c := newComposer(fmt.Sprintf("Comment on #%d %s", it.Number, it.Title), nil, "")
		c.send = func(body string) error { return postComment(repo, it.Number, body) }
		c.done = func(string) tea.Cmd { return v.after("comment posted") }
		return true, pushView(c)
	case "d":
		if it.IsPR {
			return true, pushView(newDiffView(v.repo, it.Number, it.Title))
		}
	case "s":
		if it.IsPR && v.d != nil {
			return true, pushView(newChecksView(v.repo, v.d))
		}
	case "v":
		if it.IsPR && v.d != nil {
			return true, pushView(reviewPicker(repo, it.Number, v.d.HeadSHA, loadDrafts(repo, it.Number), v.after))
		}
	case "m":
		if it.IsPR && v.d != nil && it.State == "OPEN" {
			return true, pushView(v.mergePicker())
		}
	case "D":
		if it.IsPR && v.d != nil && it.State == "OPEN" {
			id, draft := v.d.ID, !it.Draft
			word := "Mark ready for review"
			if draft {
				word = "Convert to draft"
			}
			v.ask = newConfirmation(word+fmt.Sprintf(" #%d?", it.Number), func() tea.Msg {
				if err := setDraft(id, draft); err != nil {
					return actionMsg{err: err}
				}
				return actionMsg{text: strings.ToLower(word[:1]) + word[1:] + ": done", refresh: true}
			})
		}
	case "x":
		if it.State == "MERGED" {
			return true, nil
		}
		state, word := "closed", "Close"
		if it.State == "CLOSED" {
			state, word = "open", "Reopen"
		}
		v.ask = newConfirmation(fmt.Sprintf("%s #%d?", word, it.Number), func() tea.Msg {
			if err := setState(repo, it.Number, it.IsPR, state); err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{text: fmt.Sprintf("#%d %s", it.Number, map[string]string{"closed": "closed", "open": "reopened"}[state]), refresh: true}
		})
	case "R":
		if it.IsPR && v.d != nil && it.State == "OPEN" {
			return true, pushView(reviewersPicker(repo, it.Number))
		}
	case "L":
		if v.d != nil {
			return true, pushView(labelsPicker(repo, it.Number, v.d.Labels))
		}
	case "A":
		if v.d == nil {
			return true, nil
		}
		return true, func() tea.Msg {
			var who struct {
				Viewer gqlLogin `json:"viewer"`
			}
			if err := ghQuery(`query { viewer { login } }`, nil, &who); err != nil {
				return actionMsg{err: err}
			}
			if err := assign(repo, it.Number, who.Viewer.Login); err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{text: fmt.Sprintf("assigned #%d to %s", it.Number, clean(who.Viewer.Login)), refresh: true}
		}
	case "C":
		if it.IsPR {
			c, prompt := checkoutCmd(v.repo, it.Number)
			if c == nil {
				return true, func() tea.Msg { return actionMsg{err: errors.New(prompt)} }
			}
			v.ask = newConfirmation(prompt, c)
		}
	case "W":
		if it.IsPR {
			sha := it.HeadSHA
			if v.d != nil {
				sha = v.d.HeadSHA
			}
			return true, worktreeCmd(v.repo, it.Number, sha)
		}
	case "w":
		if it.IsPR && it.State == "OPEN" && v.d != nil {
			return true, pushView(workPicker(v.repo, it, v.d))
		}
	case "J":
		return true, focusAgent(it)
	case "a":
		if inHerdr() {
			return true, pushView(newAgentPicker(v.repo, itemPrompt(repo, it, v.d)))
		}
	case "p":
		if v.d != nil && len(v.d.Linked) > 0 {
			return true, pushView(newItemView(v.repo, v.d.Linked[0]))
		}
	}
	return true, nil
}

// after reports an action done and reloads this view.
func (v *itemView) after(text string) tea.Cmd {
	return tea.Batch(func() tea.Msg { return actionMsg{text: text} }, v.fetch())
}

// mergePicker offers the merge methods the repository allows, the one you used last first, each with and without
// deleting the branch. The merge itself still asks for a typed word.
func (v *itemView) mergePicker() *picker {
	d, repo := v.d, v.repo.Slug
	var opts []option
	verbs := map[string]string{"squash": "Squash and merge", "merge": "Create a merge commit", "rebase": "Rebase and merge"}
	// Auto-merge: GitHub merges it once checks pass and reviews allow, so nobody has to wait to press the button.
	if d.AutoMerge != "" {
		opts = append(opts, option{label: "Stop merging it automatically", detail: "set by " + d.AutoBy + " to " + d.AutoMerge,
			run: func() tea.Cmd {
				return func() tea.Msg {
					if err := setAutoMerge(d.ID, "", "", false); err != nil {
						return actionMsg{err: err}
					}
					return actionMsg{text: fmt.Sprintf("#%d will not merge itself", d.Number), refresh: true}
				}
			}})
	} else if d.AutoAllowed && d.MergeWhy != "CLEAN" && d.MergeWhy != "HAS_HOOKS" && !d.Draft {
		for _, method := range d.Methods {
			method := method
			opts = append(opts, option{label: verbs[method] + ", once it can", detail: "when checks pass and reviews allow; nothing to wait for",
				run: func() tea.Cmd {
					v.ask = newConfirmation(fmt.Sprintf("%s #%d into %s once it can?", verbs[method], d.Number, d.Base), func() tea.Msg {
						if err := setAutoMerge(d.ID, method, d.HeadSHA, true); err != nil {
							return actionMsg{err: err}
						}
						return actionMsg{text: fmt.Sprintf("#%d merges itself once it can", d.Number), refresh: true}
					})
					v.ask.require = "merge"
					return nil
				}})
		}
	}
	for _, method := range d.Methods {
		for _, del := range []bool{false, true} {
			if del && d.Fork {
				continue // the branch is someone else's
			}
			method, del := method, del
			label := verbs[method]
			if del {
				label += ", then delete " + d.Head
			}
			opts = append(opts, option{label: label, run: func() tea.Cmd {
				v.ask = newConfirmation(fmt.Sprintf("%s #%d into %s?", verbs[method], d.Number, d.Base), func() tea.Msg {
					if err := mergePR(repo, d.Number, d.HeadSHA, method); err != nil {
						return actionMsg{err: err}
					}
					text := fmt.Sprintf("merged #%d", d.Number)
					if del {
						if err := deleteBranch(repo, d.Head); err != nil {
							return actionMsg{err: fmt.Errorf("merged #%d, but deleting %s: %w", d.Number, d.Head, err), refresh: true}
						}
						text += ", deleted " + d.Head
					}
					return actionMsg{text: text, refresh: true}
				})
				v.ask.require = "merge"
				return nil
			}})
		}
	}
	p := newPicker(fmt.Sprintf("Merge #%d %s", d.Number, d.Title), opts)
	p.note = func(w int) []string { return []string{" " + mergeLine(d)} }
	return p
}

// reviewPicker asks for a review's verdict, then its summary, and sends it with the line comments drafted for this
// pull request. after reports the result and reloads the view the review started from.
func reviewPicker(repo string, number int, sha string, drafts []DraftComment, after func(string) tea.Cmd) *picker {
	choice := func(label, event string, needsBody bool) option {
		return option{label: label, run: func() tea.Cmd {
			c := newComposer(fmt.Sprintf("%s · #%d", label, number), nil, "")
			if n := len(drafts); n > 0 {
				c.context = []string{sMuted.Render(fmt.Sprintf("with %d line comment(s) from the diff", n))}
			}
			c.allowEmpty = !needsBody || len(drafts) > 0
			c.send = func(body string) error {
				if err := submitReview(repo, number, sha, event, body, drafts); err != nil {
					return err
				}
				return saveDrafts(repo, number, nil)
			}
			c.done = func(string) tea.Cmd { return after("review sent: " + strings.ToLower(label)) }
			return pushView(c)
		}}
	}
	p := newPicker(fmt.Sprintf("Review #%d", number), []option{
		choice("Comment", "COMMENT", true),
		choice("Approve", "APPROVE", false),
		choice("Request changes", "REQUEST_CHANGES", true),
	})
	p.note = func(w int) []string {
		if len(drafts) == 0 {
			return []string{sMuted.Render(" No line comments drafted. In the diff, c on a line adds one.")}
		}
		lines := []string{sMuted.Render(fmt.Sprintf(" %d line comment(s) go with this review:", len(drafts)))}
		for _, d := range drafts {
			first, _, _ := strings.Cut(d.Body, "\n")
			lines = append(lines, truncStyledLine(fmt.Sprintf("   %s:%d  %s", d.Path, d.Line, first), w))
		}
		return lines
	}
	return p
}

func (v *itemView) View(w, h int) string {
	if v.lines == nil || v.lastW != w {
		v.lines, v.lastW = v.render(w), w
	}
	head := v.headLines(w)
	foot := v.footer(w)
	bodyH := max(1, h-len(head)-1-lipgloss.Height(foot))
	maxScroll := max(0, len(v.lines)-bodyH)
	v.scroll = min(v.scroll, maxScroll)
	end := min(len(v.lines), v.scroll+bodyH)
	out := append([]string{}, head...)
	out = append(out, v.lines[v.scroll:end]...)
	for i := end - v.scroll; i < bodyH; i++ {
		out = append(out, "")
	}
	status := ""
	switch {
	case v.ask != nil:
		status = v.ask.view()
	case v.err != nil:
		status = sBad.Render(" " + truncate(errText(v.err), w-2))
	case maxScroll > 0:
		status = sMuted.Render(fmt.Sprintf(" %d%%", 100*v.scroll/maxScroll))
	}
	return strings.Join(out, "\n") + "\n" + status + "\n" + foot
}

func (v *itemView) headLines(w int) []string {
	it := v.item
	title := fmt.Sprintf("#%d %s", it.Number, it.Title)
	spin := ""
	if v.loading {
		spin = sMuted.Render(" ⟳")
	}
	lines := []string{truncStyledLine(stateIcon(it)+" "+sTitle.Render(title)+spin, w)}
	meta := []string{stateWord(it), it.Author}
	if it.IsPR {
		meta = append(meta, it.Head+" → "+it.Base)
	}
	if v.d != nil {
		meta = append(meta, "opened "+age(v.d.Created)+" ago")
	}
	metaLine := " " + sMuted.Render(strings.Join(meta, " · "))
	if a, ok := agentsOn[it.Head]; ok && it.IsPR && it.State == "OPEN" {
		metaLine += "  " + agentMark(a) + sMuted.Render(fmt.Sprintf("in pane %s · J goes there", a.Pane))
	}
	lines = append(lines, truncStyledLine(metaLine, w))
	if v.d != nil && it.IsPR {
		lines = append(lines, truncStyledLine(" "+prSummary(v.d), w))
	}
	return append(lines, sRule.Render(strings.Repeat("─", w)))
}

func stateWord(it Item) string {
	switch {
	case it.State == "MERGED":
		return sMerged.Render("merged")
	case it.State == "CLOSED":
		return sBad.Render("closed")
	case it.Draft:
		return "draft"
	}
	return sGood.Render("open")
}

func prSummary(d *Detail) string {
	parts := []string{sGood.Render(fmt.Sprintf("+%d", d.Additions)) + sBad.Render(fmt.Sprintf(" −%d", d.Deletions)) +
		sMuted.Render(fmt.Sprintf(" in %d file(s)", d.Changed))}
	parts = append(parts, checksSummary(d.Checks))
	if r := reviewWord(d.Review); r != "" {
		parts = append(parts, r)
	}
	if d.State == "OPEN" {
		parts = append(parts, mergeLine(d))
	}
	return strings.Join(parts, sMuted.Render(" · "))
}

func checksSummary(checks []Check) string {
	if len(checks) == 0 {
		return sMuted.Render("no checks")
	}
	failed, running, ok := 0, 0, 0
	for _, c := range checks {
		switch checkRank(c) {
		case 0:
			failed++
		case 1:
			running++
		default:
			ok++
		}
	}
	switch {
	case failed > 0:
		return sBad.Render(fmt.Sprintf("✗ %d of %d checks failing", failed, len(checks)))
	case running > 0:
		return sWarn.Render(fmt.Sprintf("◐ %d of %d checks running", running, len(checks)))
	}
	return sGood.Render(fmt.Sprintf("✓ %d checks passed", ok))
}

// mergeLine says whether the pull request can merge, and if not, why.
func mergeLine(d *Detail) string {
	if d.AutoMerge != "" {
		return sGood.Render(fmt.Sprintf("merges itself (%s) once it can", d.AutoMerge)) + sMuted.Render(", set by "+d.AutoBy)
	}
	switch {
	case d.Mergeable == "CONFLICTING" || d.MergeWhy == "DIRTY":
		return sBad.Render("conflicts with " + d.Base)
	case d.Draft:
		return sMuted.Render("draft: not ready to merge")
	case d.MergeWhy == "CLEAN" || d.MergeWhy == "HAS_HOOKS":
		return sGood.Render("ready to merge")
	case d.MergeWhy == "BEHIND":
		return sWarn.Render("behind " + d.Base)
	case d.MergeWhy == "BLOCKED":
		return sWarn.Render("blocked by branch rules")
	case d.MergeWhy == "UNSTABLE":
		return sWarn.Render("mergeable, checks not passing")
	}
	return sMuted.Render("checking mergeability")
}

// render draws the scrolling part: details, description, checks, open threads and the conversation.
func (v *itemView) render(w int) []string {
	d := v.d
	if d == nil {
		if v.err != nil {
			return nil
		}
		return []string{sMuted.Render(" loading…")}
	}
	var out []string
	section := func(title string) {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, sBold.Render(title))
	}
	var facts []string
	if len(d.Labels) > 0 {
		facts = append(facts, sMuted.Render("labels ")+strings.Join(d.Labels, ", "))
	}
	if len(d.Assignees) > 0 {
		facts = append(facts, sMuted.Render("assigned ")+strings.Join(d.Assignees, ", "))
	}
	if d.IsPR {
		var rev []string
		for _, r := range d.Reviews {
			rev = append(rev, reviewStateIcon(r.State)+" "+r.Author)
		}
		for _, r := range d.Reviewers {
			rev = append(rev, sMuted.Render("○ "+r))
		}
		if len(rev) > 0 {
			facts = append(facts, sMuted.Render("reviewers ")+strings.Join(rev, "  "))
		}
	}
	for _, l := range d.Linked {
		facts = append(facts, sMuted.Render("closed by ")+fmt.Sprintf("#%d %s", l.Number, l.Title)+sMuted.Render(" · p"))
	}
	for _, f := range facts {
		out = append(out, truncStyledLine(" "+f, w))
	}

	section("Description")
	if strings.TrimSpace(d.Body) == "" {
		out = append(out, sMuted.Render(" No description."))
	} else {
		for _, l := range renderMarkdown(d.Body, w-2) {
			out = append(out, " "+l)
		}
	}

	if d.IsPR && len(d.Checks) > 0 {
		section("Checks  " + checksSummary(d.Checks) + sMuted.Render("  · s for all"))
		shown := 0
		for _, c := range d.Checks {
			if checkRank(c) == 2 && shown >= 3 {
				continue
			}
			if shown >= 8 {
				break
			}
			out = append(out, truncStyledLine("  "+checkIcon(c)+" "+c.Name, w))
			shown++
		}
		if rest := len(d.Checks) - shown; rest > 0 {
			out = append(out, sMuted.Render(fmt.Sprintf("  … %d more", rest)))
		}
	}

	if d.IsPR {
		var open []Thread
		for _, t := range d.Threads {
			if !t.Resolved {
				open = append(open, t)
			}
		}
		if len(open) > 0 {
			section(fmt.Sprintf("Unresolved threads (%d)", len(open)) + sMuted.Render("  · d to read them in the diff"))
			for _, t := range open {
				where := fmt.Sprintf("%s:%d", t.Path, t.Line)
				if t.Outdated {
					where += " (outdated)"
				}
				out = append(out, truncStyledLine("  "+sCode.Render(where), w))
				for _, c := range t.Comments {
					first, _, _ := strings.Cut(strings.TrimSpace(c.Body), "\n")
					out = append(out, truncStyledLine("    "+sBold.Render(c.Author)+" "+first, w))
				}
			}
		}
	}

	section("Conversation")
	if len(d.Events) == 0 {
		out = append(out, sMuted.Render(" Nothing yet. c to comment."))
	}
	for _, e := range d.Events {
		out = append(out, eventLines(e, w)...)
	}
	return out
}

func eventLines(e Event, w int) []string {
	when := sMuted.Render(" · " + age(e.At) + " ago")
	switch e.Kind {
	case "commit":
		return []string{truncStyledLine("  "+sMuted.Render("● "+e.Extra+" ")+e.Body+sMuted.Render(" · "+e.Author), w)}
	case "merged":
		return []string{"  " + sMerged.Render("◆ merged by "+e.Author) + when}
	case "closed":
		return []string{"  " + sBad.Render("● closed by "+e.Author) + when}
	case "reopened":
		return []string{"  " + sGood.Render("● reopened by "+e.Author) + when}
	case "review":
		head := "  " + reviewStateIcon(e.State) + " " + sBold.Render(e.Author) + " " + reviewVerb(e.State) + when
		if e.Extra != "" {
			head += sMuted.Render(" · " + e.Extra)
		}
		lines := []string{"", truncStyledLine(head, w)}
		for _, l := range renderMarkdown(e.Body, w-4) {
			lines = append(lines, "    "+l)
		}
		return lines
	}
	lines := []string{"", truncStyledLine("  "+sBold.Render(e.Author)+when, w)}
	for _, l := range renderMarkdown(e.Body, w-4) {
		lines = append(lines, "    "+l)
	}
	return lines
}

func reviewStateIcon(state string) string {
	switch state {
	case "APPROVED":
		return sGood.Render("✔")
	case "CHANGES_REQUESTED":
		return sBad.Render("±")
	case "DISMISSED":
		return sMuted.Render("–")
	}
	return sMuted.Render("💬")
}

func reviewVerb(state string) string {
	switch state {
	case "APPROVED":
		return sGood.Render("approved")
	case "CHANGES_REQUESTED":
		return sBad.Render("requested changes")
	case "DISMISSED":
		return sMuted.Render("review dismissed")
	case "PENDING":
		return sWarn.Render("review pending (yours, not sent)")
	}
	return sMuted.Render("reviewed")
}

func checkIcon(c Check) string {
	switch checkRank(c) {
	case 0:
		return sBad.Render("✗")
	case 1:
		return sWarn.Render("◐")
	}
	if c.Conclusion == "SKIPPED" || c.Conclusion == "NEUTRAL" {
		return sMuted.Render("–")
	}
	return sGood.Render("✓")
}

func (v *itemView) footer(w int) string {
	it := v.item
	keys := [][2]string{{"↑↓", "scroll"}, {"c", "comment"}}
	if it.IsPR {
		keys = append(keys, [2]string{"d", "diff"}, [2]string{"v", "review"}, [2]string{"s", "checks"})
		if it.State == "OPEN" {
			keys = append(keys, [2]string{"m", "merge"}, [2]string{"R", "reviewers"}, [2]string{"D", "draft/ready"})
		}
		keys = append(keys, [2]string{"C", "checkout"}, [2]string{"W", "worktree"})
		if inHerdr() && it.State == "OPEN" {
			keys = append(keys, [2]string{"w", "work on it"})
		}
	} else {
		keys = append(keys, [2]string{"A", "assign me"})
	}
	keys = append(keys, [2]string{"L", "labels"})
	if it.State != "MERGED" {
		word := "close"
		if it.State == "CLOSED" {
			word = "reopen"
		}
		keys = append(keys, [2]string{"x", word})
	}
	if inHerdr() {
		keys = append(keys, [2]string{"a", "agent"})
	}
	keys = append(keys, [2]string{"o", "web"}, [2]string{"esc", "back"})
	return keyHelp(keys, w)
}

// ---- checks ----

// checksView lists every check on a pull request's head commit. A check from Actions opens as a run, with its jobs
// and logs; any other opens its page.
type checksView struct {
	id   int
	repo Repo
	d    *Detail
	sel  int
}

func newChecksView(r Repo, d *Detail) *checksView {
	return &checksView{id: nextViewID(), repo: r, d: d}
}

func (v *checksView) ID() int       { return v.id }
func (v *checksView) Close()        {}
func (v *checksView) Init() tea.Cmd { return nil }

func (v *checksView) Update(msg tea.Msg) (bool, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return true, nil
	}
	checks := v.d.Checks
	switch k.String() {
	case "esc", "q":
		return false, nil
	case "up", "k":
		v.sel = max(0, v.sel-1)
	case "down", "j":
		v.sel = max(0, min(len(checks)-1, v.sel+1))
	case "enter", "l":
		if v.sel < len(checks) {
			c := checks[v.sel]
			if c.RunID != 0 {
				return true, pushView(newRunView(v.repo, c.RunID))
			}
			if c.URL != "" {
				return true, openURL(c.URL)
			}
		}
	case "o":
		if v.sel < len(checks) && checks[v.sel].URL != "" {
			return true, openURL(checks[v.sel].URL)
		}
	case "a":
		if inHerdr() {
			return true, pushView(newAgentPicker(v.repo, itemPrompt(v.repo.Slug, v.d.Item, v.d)))
		}
	}
	return true, nil
}

func (v *checksView) View(w, h int) string {
	lines := []string{sTitle.Render(truncate(fmt.Sprintf("Checks · #%d %s", v.d.Number, v.d.Title), w-2)),
		" " + checksSummary(v.d.Checks) + sMuted.Render(" · commit "+short(v.d.HeadSHA)), ""}
	keys := [][2]string{{"↑↓", "check"}, {"enter", "jobs & logs"}, {"o", "web"}}
	if inHerdr() {
		keys = append(keys, [2]string{"a", "agent: fix them"})
	}
	keys = append(keys, [2]string{"esc", "back"})
	help := keyHelp(keys, w)
	bodyH := max(1, h-len(lines)-lipgloss.Height(help))
	start := scrollStart(v.sel, len(v.d.Checks), bodyH)
	for i := start; i < len(v.d.Checks) && i < start+bodyH; i++ {
		c := v.d.Checks[i]
		cursor := "  "
		if i == v.sel {
			cursor = sTitle.Render("▸ ")
		}
		state := strings.ToLower(or(c.Conclusion, c.Status))
		where := ""
		if c.RunID != 0 {
			where = sMuted.Render(" · Actions")
		}
		lines = append(lines, truncStyledLine(cursor+checkIcon(c)+" "+c.Name+sMuted.Render("  "+strings.ReplaceAll(state, "_", " "))+where, w))
	}
	for len(lines) < h-lipgloss.Height(help) {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n") + "\n" + help
}

// ---- drafted line comments ----

// Line comments wait in a file until their review is sent, so closing the pane or ghdeck does not lose them.
func draftsPath(repo string, number int) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ghdeck", "drafts", fmt.Sprintf("%s_%d.json", strings.ReplaceAll(repo, "/", "_"), number)), nil
}

func loadDrafts(repo string, number int) []DraftComment {
	path, err := draftsPath(repo, number)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ds []DraftComment
	_ = json.Unmarshal(b, &ds)
	for i := range ds {
		ds[i].Path, ds[i].Side = clean(ds[i].Path), clean(ds[i].Side)
	}
	return ds
}

func saveDrafts(repo string, number int, ds []DraftComment) error {
	path, err := draftsPath(repo, number)
	if err != nil {
		return err
	}
	if len(ds) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
