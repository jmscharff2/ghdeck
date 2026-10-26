package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// A new pull request takes four steps: the branch to merge (yours, or any branch on GitHub, staging say), the branch
// it goes into, what that would bring, and then the title and description. ghdeck has no settings for how a team
// works, so it reads that from where the repository's recent pull requests went: features into develop, develop into
// staging, staging into main, or everything straight to main.

// newPRFlow starts a new pull request, offering the checked-out branch first.
func newPRFlow(r Repo, current string) *picker {
	p := newPicker("New pull request · the branch to merge", nil)
	p.filterable = true
	var info BranchInfo // set by load; read once it has loaded
	left := 0           // merged branches left out, the same
	p.note = func(int) []string {
		if p.loading || left == 0 {
			return nil
		}
		return []string{sMuted.Render(fmt.Sprintf(" %s merged and unchanged since left out; typing finds them.", plural(left, "branch")))}
	}
	// Only a hundred branches are listed; any other can be named.
	p.extra = func(typed string) []option {
		return []option{{label: "use " + typed, detail: "a branch not listed here",
			run: func() tea.Cmd { return pushView(basePicker(r, info, typed)) }}}
	}
	p.load = func() ([]option, error) {
		var err error
		if info, err = fetchBranches(r.Slug); err != nil {
			return nil, err
		}
		var opts []option
		if current != "" && current != info.Default {
			if o, err := currentOption(r, &info, current); err != nil {
				return nil, err
			} else {
				opts = append(opts, o)
			}
		}
		merged := mergedBranches(info)
		hidden := 0
		for _, b := range info.Branches {
			if b.Name == current && current != info.Default {
				continue
			}
			b := b
			detail := short(b.SHA) + " · " + age(b.Date) + " ago"
			if b.Name == info.Default {
				detail += " · the default branch"
			}
			o := option{label: b.Name, match: b.Name, detail: detail,
				run: func() tea.Cmd { return pushView(basePicker(r, info, b.Name)) }}
			if n, ok := merged[b.Name]; ok { // done with: out of the way, but typing finds it
				o.hidden, o.detail = true, detail+fmt.Sprintf(" · merged in #%d", n)
				hidden++
			}
			opts = append(opts, o)
		}
		left = hidden
		return opts, nil
	}
	return p
}

// currentOption offers the checked-out branch: from GitHub when it is there, or through gh pr create, which pushes a
// branch first and opens pull requests from forks, when it is not.
func currentOption(r Repo, info *BranchInfo, current string) (option, error) {
	found := false
	for _, b := range info.Branches {
		found = found || b.Name == current
	}
	if !found { // past the hundred branches listed, perhaps
		b, ok, err := fetchBranch(r.Slug, current)
		if err != nil {
			return option{}, err
		}
		if ok {
			info.Branches = append(info.Branches, b)
			found = true
		}
	}
	if found {
		return option{label: current + sGood.Render("  your branch"), match: current, detail: "checked out here",
			run: func() tea.Cmd { return pushView(basePicker(r, *info, current)) }}, nil
	}
	ghCreate := func() tea.Cmd {
		return openPane("new pull request", or(r.Root, "."), "gh pr create -R "+shellQuote(r.Slug), true)
	}
	if fork := upstreamRepo(r.Root, current); fork != "" && !strings.EqualFold(fork, r.Slug) {
		return option{label: current + sMuted.Render("  your branch, on "+fork), match: current,
			detail: "gh pr create opens it from the fork, in a pane", run: ghCreate}, nil
	}
	return option{label: current + sWarn.Render("  your branch, not pushed yet"), match: current,
		detail: "gh pr create pushes it and asks the rest, in a pane", run: ghCreate}, nil
}

// mergedBranches finds branches done with: their last pull request merged, with nothing pushed since. It names the
// pull request for each.
func mergedBranches(info BranchInfo) map[string]int {
	tip := map[string]string{}
	for _, b := range info.Branches {
		tip[b.Name] = b.SHA
	}
	ll := longLived(info)
	out, seen := map[string]int{}, map[string]bool{}
	for _, pr := range info.Recent { // the most recent first, so a branch's last pull request decides
		if pr.Fork || seen[pr.Head] {
			continue
		}
		seen[pr.Head] = true
		if pr.Merged && !ll[pr.Head] && pr.HeadSHA != "" && tip[pr.Head] == pr.HeadSHA {
			out[pr.Head] = pr.Number
		}
	}
	return out
}

// longLived is the branches pull requests go into: the default, every recent base, and any with a usual name for one
// (staging, develop). Everything else is taken for a feature branch.
func longLived(info BranchInfo) map[string]bool {
	ll := map[string]bool{}
	if info.Default != "" {
		ll[info.Default] = true
	}
	for _, b := range info.Branches {
		for _, u := range usualBranches {
			ll[b.Name] = ll[b.Name] || b.Name == u
		}
	}
	for _, pr := range info.Recent {
		if !pr.Fork {
			ll[pr.Base] = true
		}
	}
	return ll
}

type baseChoice struct{ name, why string }

// guessBases orders the branches head could go into, the likeliest first, each saying why:
//   - where head itself went last time: staging into main
//   - where its family went: release/1.5 where release/1.4 went
//   - where feature branches go, for a feature branch: develop, in a repository that works through develop
//   - then the default branch, the other long-lived branches, and the rest.
func guessBases(info BranchInfo, head string) []baseChoice {
	ll := longLived(info)
	var out []baseChoice
	seen := map[string]bool{head: true}
	add := func(name, why string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		if name == info.Default && !strings.Contains(why, "default") {
			why += " · the default branch"
		}
		out = append(out, baseChoice{name, why})
	}
	family, _, hasFamily := strings.Cut(head, "/")
	for _, pr := range info.Recent { // the most recent first
		if !pr.Fork && pr.Head == head {
			add(pr.Base, fmt.Sprintf("where #%d from %s went", pr.Number, head))
			break
		}
	}
	if hasFamily {
		for _, pr := range info.Recent {
			if f, _, ok := strings.Cut(pr.Head, "/"); ok && f == family && !pr.Fork && pr.Head != head {
				add(pr.Base, fmt.Sprintf("where %s (#%d) went", pr.Head, pr.Number))
				break
			}
		}
	}
	if !ll[head] {
		counts, total, best := map[string]int{}, 0, ""
		for _, pr := range info.Recent {
			if !pr.Fork && !ll[pr.Head] {
				counts[pr.Base]++
				total++
				if best == "" || counts[pr.Base] > counts[best] {
					best = pr.Base
				}
			}
		}
		if best != "" {
			add(best, fmt.Sprintf("where %d of the last %d feature pull requests went", counts[best], total))
		}
	}
	add(info.Default, "the default branch")
	for _, b := range info.Branches {
		if ll[b.Name] {
			add(b.Name, "a branch pull requests go into · "+age(b.Date)+" ago")
		}
	}
	for _, b := range info.Branches {
		add(b.Name, short(b.SHA)+" · "+age(b.Date)+" ago")
	}
	return out
}

// basePicker asks which branch head goes into, the likeliest first. It looks up head's own past pull requests first,
// which the latest fifty can miss.
func basePicker(r Repo, info BranchInfo, head string) *picker {
	p := newPicker(fmt.Sprintf("New pull request · merge %s into", head), nil)
	p.filterable = true
	withPast := info // and head's own past pull requests, once loaded
	p.extra = func(typed string) []option {
		return []option{{label: "use " + typed, detail: "a branch not listed here",
			run: func() tea.Cmd { return pushView(comparePicker(r, withPast, typed, head)) }}}
	}
	p.load = func() ([]option, error) {
		past, err := fetchHeadHistory(r.Slug, head)
		if err != nil {
			return nil, err
		}
		// The order guessBases reads them in; a repeat changes nothing.
		withPast.Recent = append(append([]RecentPR{}, past...), info.Recent...)
		var opts []option
		merged := mergedBranches(withPast)
		for _, c := range guessBases(withPast, head) {
			base := c.name
			_, done := merged[base]
			opts = append(opts, option{label: base, match: base, detail: c.why, hidden: done,
				run: func() tea.Cmd { return pushView(comparePicker(r, withPast, base, head)) }})
		}
		return opts, nil
	}
	return p
}

// backwards says merging head into base looks like the wrong way round: a long-lived branch into a feature branch.
func backwards(info BranchInfo, base, head string) bool {
	ll := longLived(info)
	return ll[head] && !ll[base]
}

// comparePicker shows what merging head into base would bring, and offers to write the pull request; or, when one
// is open between them already, to open that instead.
func comparePicker(r Repo, info BranchInfo, base, head string) *picker {
	p := newPicker(fmt.Sprintf("New pull request · %s → %s", head, base), nil)
	var c *Comparison // set by load, read by note once it has loaded
	p.note = func(w int) []string {
		if c == nil {
			return nil
		}
		var lines []string
		switch {
		case c.Ahead == 0:
			lines = append(lines, sWarn.Render(fmt.Sprintf(" %s has nothing that %s does not have already.", head, base)))
		default:
			lines = append(lines, fmt.Sprintf(" %s, %s · %s", plural(c.Ahead, "commit"), plural(c.Files, "file"),
				sGood.Render(fmt.Sprintf("+%d", c.Additions))+sBad.Render(fmt.Sprintf("−%d", c.Removed))))
			if c.Behind > 0 {
				lines = append(lines, sWarn.Render(fmt.Sprintf(" %s is %s behind %s, so they may conflict.", head, plural(c.Behind, "commit"), base)))
			}
		}
		if backwards(info, base, head) {
			lines = append(lines, sWarn.Render(fmt.Sprintf(" This merges %s, a branch pull requests go into, into %s, which looks like a feature branch. Usually it goes the other way.", head, base)))
		}
		if c.Open != nil {
			lines = append(lines, sWarn.Render(fmt.Sprintf(" #%d is open from %s into %s already: %s", c.Open.Number, head, base, c.Open.Title)))
		}
		if n := len(c.Commits); n > 0 {
			lines = append(lines, "")
			show := c.Commits
			if n > 8 {
				show = c.Commits[n-8:] // the newest
				lines = append(lines, sMuted.Render(fmt.Sprintf("   … %d older", n-8)))
			}
			for _, cm := range show {
				lines = append(lines, truncStyledLine(fmt.Sprintf("   %s  %s  %s", sMuted.Render(short(cm.SHA)), cm.Subject, sMuted.Render(cm.Author)), w))
			}
		}
		var wrapped []string
		for _, l := range lines {
			wrapped = append(wrapped, wrapStyledText(l, w-1, " ")...)
		}
		return wrapped
	}
	p.load = func() ([]option, error) {
		cmp, err := fetchComparison(r.Slug, base, head)
		if err != nil {
			return nil, err
		}
		c = cmp
		var opts []option
		if cmp.Open != nil {
			it := *cmp.Open
			opts = append(opts, option{label: fmt.Sprintf("Open #%d", it.Number), detail: "the pull request already open",
				run: func() tea.Cmd { return pushView(newItemView(r, it)) }})
		}
		if cmp.Ahead > 0 && cmp.Open == nil {
			opts = append(opts,
				option{label: "Write the pull request", detail: "then it opens, ready for review",
					run: func() tea.Cmd { return pushView(prComposer(r, base, head, cmp, false)) }},
				option{label: "Write it as a draft", detail: "nobody is asked to review it until you mark it ready",
					run: func() tea.Cmd { return pushView(prComposer(r, base, head, cmp, true)) }})
		}
		return opts, nil
	}
	return p
}

// prComposer writes the title, on the first line, and the description beneath, and creates the pull request.
func prComposer(r Repo, base, head string, cmp *Comparison, draft bool) *composer {
	title, body := prDraft(head, base, cmp, prTemplate(r.Root))
	what := "New pull request"
	if draft {
		what = "New draft pull request"
	}
	c := newComposer(fmt.Sprintf("%s · %s → %s", what, head, base),
		[]string{sMuted.Render("The first line is the title; the rest is the description.")}, title+"\n\n"+body)
	var made Item // set by send, read by done after it
	c.send = func(text string) error {
		t, b, _ := strings.Cut(text, "\n")
		if t = strings.TrimSpace(t); t == "" {
			return fmt.Errorf("the first line is the title: write one")
		}
		it, err := createPR(r.Slug, base, head, t, strings.TrimSpace(b), draft)
		made = it
		return err
	}
	c.done = func(string) tea.Cmd {
		it := made
		// Then who should review it, GitHub's suggestions first; esc leaves it to later (R).
		return tea.Sequence(
			func() tea.Msg {
				return actionMsg{text: fmt.Sprintf("created #%d · esc to skip reviewers", it.Number), refresh: true}
			},
			pushView(newItemView(r, it)),
			func() tea.Msg {
				p := reviewersPicker(r.Slug, it.Number)
				p.skipEmpty = true // nobody to ask, in a repository of one
				return pushViewMsg{p}
			})
	}
	return c
}

// prDraft suggests a title and description. One commit gives its own message. More give a title naming the branches
// and, for a promotion like staging into main, the pull requests it carries, as release notes; any commits that came
// some other way follow. A repository's template comes first.
func prDraft(head, base string, cmp *Comparison, template string) (title, body string) {
	var parts []string
	if template != "" {
		parts = append(parts, template)
	}
	if len(cmp.Commits) == 1 {
		title = cmp.Commits[0].Subject
		if b := cmp.Commits[0].Body; b != "" {
			parts = append(parts, b)
		}
		return title, strings.Join(parts, "\n\n")
	}
	title = fmt.Sprintf("Merge %s into %s", head, base)
	prs, others := releaseNotes(cmp.Commits)
	if len(prs) > 0 {
		parts = append(parts, "## Pull requests\n\n"+strings.Join(prs, "\n"))
	}
	if len(others) > 0 {
		heading := "## Commits"
		if len(prs) > 0 {
			heading = "## Other commits"
		}
		list := []string{heading, ""}
		for _, cm := range others {
			list = append(list, fmt.Sprintf("- %s (%s)", cm.Subject, short(cm.SHA)))
		}
		parts = append(parts, strings.Join(list, "\n"))
	}
	if more := cmp.Ahead - len(cmp.Commits); more > 0 {
		parts = append(parts, fmt.Sprintf("…and %d more commits than GitHub lists in a comparison.", more))
	}
	return title, strings.Join(parts, "\n\n")
}

var (
	mergedPR = regexp.MustCompile(`^Merge pull request #(\d+) from `)
	squashPR = regexp.MustCompile(`\(#(\d+)\)$`)
)

// releaseNotes reads the pull requests a run of commits carries, in the order they landed: a merge commit names its
// pull request, and a squashed one ends with its number. The commits inside a merged pull request are its own, so
// they are left out; other merges (main back into staging, say) bring nothing new and are left out too. Whatever
// came some other way, pushed straight to the branch, is in others.
func releaseNotes(commits []Commit) (prs []string, others []Commit) {
	byID := map[string]Commit{}
	isParent := map[string]bool{}
	for _, c := range commits {
		byID[c.SHA] = c
		for _, p := range c.Parents {
			isParent[p] = true
		}
	}
	// The tip is the commit nothing else in the range builds on; the branch's own history is its first parents.
	var tip string
	for _, c := range commits {
		if !isParent[c.SHA] {
			tip = c.SHA
		}
	}
	var line []Commit
	onLine := map[string]bool{}
	for sha := tip; sha != ""; {
		c, ok := byID[sha]
		if !ok || onLine[sha] {
			break
		}
		line, onLine[sha] = append(line, c), true
		sha = ""
		if len(c.Parents) > 0 {
			sha = c.Parents[0]
		}
	}
	seen := map[string]bool{}
	inside := map[string]bool{}           // commits a merge brought in, under its first parent
	for i := len(line) - 1; i >= 0; i-- { // the oldest first
		c := line[i]
		switch {
		case len(c.Parents) > 1:
			if m := mergedPR.FindStringSubmatch(c.Subject); m != nil && !seen[m[1]] {
				seen[m[1]] = true
				title, _, _ := strings.Cut(c.Body, "\n")
				prs = append(prs, fmt.Sprintf("- %s (#%s)", or(strings.TrimSpace(title), "pull request"), m[1]))
			}
			todo := append([]string{}, c.Parents[1:]...)
			for len(todo) > 0 {
				sha := todo[len(todo)-1]
				todo = todo[:len(todo)-1]
				if inside[sha] || onLine[sha] {
					continue
				}
				if pc, ok := byID[sha]; ok {
					inside[sha] = true
					todo = append(todo, pc.Parents...)
				}
			}
		case squashPR.MatchString(c.Subject):
			if n := squashPR.FindStringSubmatch(c.Subject)[1]; !seen[n] {
				seen[n] = true
				prs = append(prs, "- "+c.Subject)
			}
		}
	}
	// Everything else, in the order GitHub lists it: including any the first-parent walk could not reach, when a
	// comparison past GitHub's 250 commits leaves the history incomplete.
	for _, c := range commits {
		if inside[c.SHA] || len(c.Parents) > 1 || onLine[c.SHA] && squashPR.MatchString(c.Subject) {
			continue
		}
		others = append(others, c)
	}
	return prs, others
}

// prTemplate reads the repository's pull request template from the checkout, in any of the places GitHub looks.
func prTemplate(root string) string {
	if root == "" {
		return ""
	}
	for _, dir := range []string{".github", ".", "docs"} {
		for _, name := range []string{"pull_request_template.md", "PULL_REQUEST_TEMPLATE.md"} {
			b, err := os.ReadFile(filepath.Join(root, dir, name))
			if err == nil && len(b) < 64<<10 {
				return strings.TrimSpace(cleanBody(string(b)))
			}
		}
	}
	return ""
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	for _, end := range []string{"ch", "sh", "s", "x"} {
		if strings.HasSuffix(word, end) {
			return fmt.Sprintf("%d %ses", n, word)
		}
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// reviewersPicker asks for reviewers: GitHub's suggestions first, those asked already ticked. The view underneath
// reloads after.
func reviewersPicker(repo string, number int) *picker {
	var all []Reviewer // set by load, read by done once it has loaded
	p := newMultiPicker(fmt.Sprintf("Reviewers for #%d", number), nil, nil)
	p.load = func() ([]option, error) {
		rs, err := fetchReviewers(repo, number)
		if err != nil {
			return nil, err
		}
		all = rs
		var opts []option
		for _, r := range rs {
			label, detail := r.Login, r.Name
			if r.Team {
				label = "team " + r.Login
			}
			if r.Why != "" {
				detail = strings.TrimPrefix(detail+" · ", " · ") + sGood.Render("suggested: "+r.Why)
			}
			if r.Reviewed != "" {
				detail = strings.TrimPrefix(detail+" · ", " · ") + sWarn.Render(r.Reviewed+" · tick to ask again")
			}
			opts = append(opts, option{label: label, match: r.key(), detail: detail, on: r.Requested})
		}
		return opts, nil
	}
	p.done = func(ticked []string) tea.Cmd {
		want := map[string]bool{}
		for _, k := range ticked {
			want[k] = true
		}
		var add, remove []Reviewer
		for _, r := range all {
			switch {
			case want[r.key()] && !r.Requested:
				add = append(add, r)
			case !want[r.key()] && r.Requested:
				remove = append(remove, r)
			}
		}
		if len(add)+len(remove) == 0 {
			return nil
		}
		return func() tea.Msg {
			if err := setReviewers(repo, number, add, remove); err != nil {
				return actionMsg{err: err}
			}
			var names []string
			for _, r := range add {
				names = append(names, r.Login)
			}
			text := "asked " + strings.Join(names, ", ") + " to review"
			if len(add) == 0 {
				text = fmt.Sprintf("stopped asking %s", plural(len(remove), "reviewer"))
			}
			return actionMsg{text: text, refresh: true}
		}
	}
	return p
}

// labelsPicker sets a pull request's or issue's labels, those on it already ticked.
func labelsPicker(repo string, number int, current []string) *picker {
	p := newMultiPicker(fmt.Sprintf("Labels for #%d", number), current, nil)
	p.load = func() ([]option, error) {
		ls, err := fetchLabels(repo)
		if err != nil {
			return nil, err
		}
		var opts []option
		for _, l := range ls {
			opts = append(opts, option{label: l.Name, match: l.Name, detail: l.Description})
		}
		return opts, nil
	}
	p.done = func(ticked []string) tea.Cmd {
		had := map[string]bool{}
		for _, l := range current {
			had[l] = true
		}
		same := len(ticked) == len(current)
		for _, l := range ticked {
			same = same && had[l]
		}
		if same {
			return nil
		}
		return func() tea.Msg {
			if err := setLabels(repo, number, ticked); err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{text: fmt.Sprintf("labels: %s", or(strings.Join(ticked, ", "), "none")), refresh: true}
		}
	}
	return p
}
