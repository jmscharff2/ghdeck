package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The three lists, in tab order.
const (
	tabPRs = iota
	tabIssues
	tabRuns
)

var tabNames = []string{"Pull requests", "Issues", "Actions"}

// Data keys.
const (
	kStatus = "status"
	kPRs    = "prs"
	kIssues = "issues"
	kRuns   = "runs"
)

type panel struct {
	val     any
	err     error
	loading bool
	at      time.Time
	query   string // the search a list was loaded with, so a stale answer is dropped
}

type dataMsg struct {
	key   string
	query string
	val   any
	err   error
}

// actionMsg reports something done: shown as a toast, and refresh reloads what is on screen.
type actionMsg struct {
	text    string
	err     error
	refresh bool
}

type tickMsg struct{ gen int }

// wakeMsg comes with the first key after ghdeck sat idle: refresh at once, and go back to the usual pace.
type wakeMsg struct{}

type refreshNowMsg struct{}

// refresher is a view that reloads when an action changes what it shows.
type refresher interface{ Refresh() tea.Cmd }

type model struct {
	repo     Repo
	branch   string
	panels   map[string]*panel
	tab      int
	sel      [3]int
	filter   [2]int    // the preset search for pull requests and issues
	search   [2]string // a search typed with /, replacing the preset
	runsMine bool      // Actions shows only the current branch's runs

	typing bool
	draft  string

	w, h    int
	flash   string
	isErr   bool
	flashAt time.Time
	confirm *confirmation
	help    bool
	stack   []subview
	tickGen int // which tick is current, so waking up does not leave a slow one running beside it

	// openBranch opens the branch's pull request as soon as the first status says which it is.
	openBranch bool

	// seen remembers what was already there, so only news is announced.
	seenReviews map[int]bool
	lastBranch  *Item
}

func newModel(r Repo) model {
	return model{repo: r, branch: currentBranch(r.Root), panels: map[string]*panel{}}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.refreshStatus(), m.refreshTab(tabPRs), m.tick())
}

func load(key, query string, f func() (any, error)) tea.Cmd {
	return func() tea.Msg {
		v, err := f()
		return dataMsg{key: key, query: query, val: v, err: err}
	}
}

func (m model) mark(key, query string) {
	p := m.panels[key]
	if p == nil {
		p = &panel{}
		m.panels[key] = p
	}
	p.loading = true
	if p.query != query {
		p.query = query
		p.val, p.err = nil, nil // a different search: the old results do not answer it
	}
}

func (m model) refreshStatus() tea.Cmd {
	repo, branch := m.repo.Slug, m.branch
	m.mark(kStatus, "")
	return load(kStatus, "", func() (any, error) { return fetchStatus(repo, branch) })
}

func (m model) listQuery(tab int) string {
	if tab == tabRuns {
		if m.runsMine {
			return m.branch
		}
		return ""
	}
	fs := prFilters
	if tab == tabIssues {
		fs = issueFilters
	}
	return searchQuery(m.repo.Slug, tab == tabPRs, fs[m.filter[tab]], m.search[tab])
}

func (m model) refreshTab(tab int) tea.Cmd {
	q := m.listQuery(tab)
	switch tab {
	case tabRuns:
		repo := m.repo.Slug
		m.mark(kRuns, q)
		return load(kRuns, q, func() (any, error) { return fetchRuns(repo, q) })
	case tabIssues:
		m.mark(kIssues, q)
		return load(kIssues, q, func() (any, error) { return fetchList(q) })
	}
	m.mark(kPRs, q)
	return load(kPRs, q, func() (any, error) { return fetchList(q) })
}

// tick refreshes the list every 30 seconds, every 10 while an Actions run on screen is moving, and less often while
// ghdeck sits idle or the hourly budget runs low (see pace). The header refreshes once a minute at most. Nothing
// refreshes behind a full-screen view but the header; each view keeps its own time.
func (m model) tick() tea.Cmd {
	every := 30 * time.Second
	if m.tab == tabRuns {
		for _, r := range m.runs() {
			if r.Status != "completed" {
				every = 10 * time.Second
			}
		}
	}
	gen := m.tickGen
	return tea.Tick(pace(every), func(time.Time) tea.Msg { return tickMsg{gen} })
}

func (m model) list(tab int) (List, *panel) {
	key := []string{kPRs, kIssues}[tab]
	p := m.panels[key]
	if p == nil {
		return List{}, nil
	}
	l, _ := p.val.(List)
	return l, p
}

func (m model) runs() []Run {
	if p := m.panels[kRuns]; p != nil {
		r, _ := p.val.([]Run)
		return r
	}
	return nil
}

func (m model) status() Status {
	if p := m.panels[kStatus]; p != nil {
		s, _ := p.val.(Status)
		return s
	}
	return Status{}
}

func (m model) rows() int {
	if m.tab == tabRuns {
		return len(m.runs())
	}
	l, _ := m.list(m.tab)
	return len(l.Items)
}

func (m model) selected() (Item, bool) {
	if m.tab == tabRuns {
		return Item{}, false
	}
	l, _ := m.list(m.tab)
	if s := m.sel[m.tab]; s < len(l.Items) {
		return l.Items[s], true
	}
	return Item{}, false
}

func (m model) authExpired() bool {
	for _, p := range m.panels {
		if p.err != nil && errors.Is(p.err, errAuth) {
			return true
		}
	}
	return false
}

func (m *model) say(text string, isErr bool) { m.flash, m.isErr, m.flashAt = text, isErr, time.Now() }

// Update notes each time someone uses ghdeck, and wakes it after a quiet spell.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyMsg, tea.MouseMsg:
		woke := idle()
		lastActive = time.Now()
		if woke {
			next, cmd := m.update(msg)
			return next, tea.Batch(cmd, func() tea.Msg { return wakeMsg{} })
		}
	}
	return m.update(msg)
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pushViewMsg:
		m.stack = append(m.stack, msg.v)
		return m, msg.v.Init()
	case viewMsg:
		for _, v := range m.stack {
			if v.ID() == msg.viewID() {
				open, cmd := v.Update(msg)
				if !open {
					m = m.closeView(v)
				}
				return m, cmd
			}
		}
		return m, nil
	case tea.KeyMsg:
		if len(m.stack) == 0 {
			break
		}
		if msg.String() == "ctrl+c" {
			for _, v := range m.stack {
				v.Close()
			}
			return m, tea.Quit
		}
		top := m.stack[len(m.stack)-1]
		open, cmd := top.Update(msg)
		if !open {
			m = m.closeView(top)
		}
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		if msg.gen != m.tickGen {
			return m, nil
		}
		m.branch = currentBranch(m.repo.Root)
		cmds := []tea.Cmd{m.tick()}
		// The header asks GraphQL too, so it keeps to a minute even while the list goes faster.
		if p := m.panels[kStatus]; p == nil || !p.loading && time.Since(p.at) > pace(time.Minute)-5*time.Second {
			cmds = append(cmds, m.refreshStatus())
		}
		if len(m.stack) == 0 {
			cmds = append(cmds, m.refreshTab(m.tab))
		}
		return m, tea.Batch(cmds...)
	case wakeMsg:
		m.tickGen++
		m.branch = currentBranch(m.repo.Root)
		cmds := []tea.Cmd{m.refreshStatus(), m.tick()}
		if n := len(m.stack); n == 0 {
			cmds = append(cmds, m.refreshTab(m.tab))
		} else if r, ok := m.stack[n-1].(refresher); ok {
			cmds = append(cmds, r.Refresh())
		}
		return m, tea.Batch(cmds...)
	case refreshNowMsg:
		cmds := []tea.Cmd{m.refreshStatus(), m.refreshTab(m.tab)}
		if n := len(m.stack); n > 0 {
			if r, ok := m.stack[n-1].(refresher); ok {
				cmds = append(cmds, r.Refresh())
			}
		}
		return m, tea.Batch(cmds...)
	case dataMsg:
		p := m.panels[msg.key]
		if p == nil || p.query != msg.query {
			return m, nil // an answer to a search since replaced
		}
		before := p.val
		p.loading, p.at = false, time.Now()
		if msg.err != nil {
			p.err = msg.err // keep the last good value on screen beside the error
		} else {
			p.val, p.err = msg.val, nil
		}
		if n := m.rows(); m.sel[m.tab] >= n {
			m.sel[m.tab] = max(0, n-1)
		}
		if msg.key == kStatus && msg.err == nil {
			cmd := m.announce(before)
			if m.openBranch {
				m.openBranch = false
				if pr := m.status().BranchPR; pr != nil {
					return m, tea.Batch(cmd, pushView(newItemView(m.repo, *pr)))
				}
				m.say("no open pull request for "+or(m.branch, "this checkout")+" · n to create one", false)
			}
			return m, cmd
		}
	case actionMsg:
		if msg.err != nil {
			m.say(errText(msg.err), true)
		} else if msg.text != "" {
			m.say(msg.text, false)
		}
		if msg.refresh {
			// GitHub takes a moment to show what changed. A one-off refresh: the tick keeps its own schedule.
			return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return refreshNowMsg{} })
		}
	case execMsg:
		return m, execHere(msg)
	case tea.KeyMsg:
		if m.confirm != nil {
			return m.updateConfirm(msg)
		}
		if m.typing {
			return m.updateSearch(msg)
		}
		return m.updateKey(msg)
	}
	return m, nil
}

// announce tells you when your branch's pull request moves on (checks finish, a review comes in) and when someone
// asks for your review. Only changes seen while ghdeck runs count: the first load is remembered, not announced.
func (m *model) announce(before any) tea.Cmd {
	s := m.status()
	prev, had := before.(Status)
	var cmds []tea.Cmd
	if m.seenReviews == nil {
		m.seenReviews = map[int]bool{}
		for _, it := range s.ReviewFor {
			m.seenReviews[it.Number] = true
		}
	} else {
		for _, it := range s.ReviewFor {
			if !m.seenReviews[it.Number] {
				m.seenReviews[it.Number] = true
				cmds = append(cmds, notify("Review requested", fmt.Sprintf("#%d %s by %s", it.Number, it.Title, it.Author), false))
			}
		}
	}
	if had && prev.BranchPR != nil && s.BranchPR != nil && prev.BranchPR.Number == s.BranchPR.Number {
		a, b := prev.BranchPR, s.BranchPR
		what := fmt.Sprintf("#%d %s", b.Number, b.Title)
		if a.Checks != b.Checks {
			switch {
			case checksFailed(b.Checks):
				cmds = append(cmds, notify("Checks failed", what, false))
			case b.Checks == "SUCCESS" && a.Checks != "":
				cmds = append(cmds, notify("Checks passed", what, true))
			}
		}
		if a.Review != b.Review {
			switch b.Review {
			case "APPROVED":
				cmds = append(cmds, notify("Approved", what, true))
			case "CHANGES_REQUESTED":
				cmds = append(cmds, notify("Changes requested", what, false))
			}
		}
	}
	return tea.Batch(cmds...)
}

func checksFailed(state string) bool { return state == "FAILURE" || state == "ERROR" }

func (m model) closeView(v subview) model {
	v.Close()
	for i, s := range m.stack {
		if s == v {
			m.stack = append(m.stack[:i:i], m.stack[i+1:]...)
			break
		}
	}
	// What was underneath may be out of date: an action in the closed view changed it.
	return m
}

func (m model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	done, ok := m.confirm.key(msg)
	if !done {
		return m, nil
	}
	cmd := m.confirm.cmd
	m.confirm = nil
	if !ok {
		m.say("cancelled", false)
		return m, nil
	}
	return m, cmd
}

func (m model) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		m.typing = false
		m.search[m.tab] = strings.TrimSpace(m.draft)
		m.sel[m.tab] = 0
		return m, m.refreshTab(m.tab)
	case tea.KeyEsc:
		m.typing = false
	case tea.KeyBackspace:
		if r := []rune(m.draft); len(r) > 0 {
			m.draft = string(r[:len(r)-1])
		}
	case tea.KeyCtrlU:
		m.draft = ""
	case tea.KeyRunes, tea.KeySpace:
		m.draft += string(msg.Runes)
	}
	return m, nil
}

func (m model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.help = !m.help
	case "tab", "right", "L":
		return m.switchTab((m.tab + 1) % len(tabNames))
	case "shift+tab", "left", "H":
		return m.switchTab((m.tab + len(tabNames) - 1) % len(tabNames))
	case "1", "2", "3":
		return m.switchTab(int(key[0] - '1'))
	case "r":
		m.say("refreshing", false)
		return m, tea.Batch(m.refreshStatus(), m.refreshTab(m.tab))
	case "j", "down":
		if m.sel[m.tab] < m.rows()-1 {
			m.sel[m.tab]++
		}
	case "k", "up":
		if m.sel[m.tab] > 0 {
			m.sel[m.tab]--
		}
	case "g", "home":
		m.sel[m.tab] = 0
	case "G", "end":
		m.sel[m.tab] = max(0, m.rows()-1)
	case "pgdown", "ctrl+d", " ":
		m.sel[m.tab] = min(max(0, m.rows()-1), m.sel[m.tab]+10)
	case "pgup", "ctrl+u":
		m.sel[m.tab] = max(0, m.sel[m.tab]-10)
	case "f":
		if m.tab == tabRuns {
			m.runsMine = !m.runsMine
			m.sel[m.tab] = 0
			return m, m.refreshTab(m.tab)
		}
		n := len(prFilters)
		if m.tab == tabIssues {
			n = len(issueFilters)
		}
		m.filter[m.tab] = (m.filter[m.tab] + 1) % n
		m.search[m.tab], m.sel[m.tab] = "", 0
		return m, m.refreshTab(m.tab)
	case "/":
		if m.tab != tabRuns {
			m.typing, m.draft = true, m.search[m.tab]
		}
	case "esc":
		if m.tab != tabRuns && m.search[m.tab] != "" {
			m.search[m.tab], m.sel[m.tab] = "", 0
			return m, m.refreshTab(m.tab)
		}
	case "enter":
		if m.tab == tabRuns {
			if runs := m.runs(); m.sel[m.tab] < len(runs) {
				return m, pushView(newRunView(m.repo, runs[m.sel[m.tab]].ID))
			}
			return m, nil
		}
		if it, ok := m.selected(); ok {
			return m, pushView(newItemView(m.repo, it))
		}
	case "d":
		if it, ok := m.selected(); ok && it.IsPR {
			return m, pushView(newDiffView(m.repo, it.Number, it.Title))
		}
	case "b":
		if pr := m.status().BranchPR; pr != nil {
			return m, pushView(newItemView(m.repo, *pr))
		}
		if m.branch == "" {
			m.say("not on a branch", true)
		} else {
			m.say("no open pull request for "+m.branch+" · n to create one", false)
		}
	case "o":
		if m.tab == tabRuns {
			if runs := m.runs(); m.sel[m.tab] < len(runs) {
				return m, openURL(runs[m.sel[m.tab]].URL)
			}
			return m, nil
		}
		if it, ok := m.selected(); ok {
			return m, openURL(it.URL)
		}
	case "n":
		switch m.tab {
		case tabPRs:
			return m, pushView(newPRFlow(m.repo, m.branch))
		case tabIssues:
			return m, openPane("new issue", m.dir(), "gh issue create -R "+shellQuote(m.repo.Slug), true)
		}
	case "C":
		if it, ok := m.selected(); ok && it.IsPR {
			c, msg := checkoutCmd(m.repo, it.Number)
			if c == nil {
				m.say(msg, true)
				return m, nil
			}
			m.confirm = newConfirmation(msg, c)
		}
	case "W":
		if it, ok := m.selected(); ok && it.IsPR {
			return m, worktreeCmd(m.repo, it.Number)
		}
	case "a":
		if !inHerdr() {
			m.say("handing work to an agent needs Herdr", true)
			return m, nil
		}
		if m.tab == tabRuns {
			return m, nil
		}
		if it, ok := m.selected(); ok {
			return m, pushView(newAgentPicker(m.repo, itemPrompt(m.repo.Slug, it, nil)))
		}
	case "A":
		return m, openPane("sign in", m.dir(), "gh auth login && echo 'Signed in. Press r in ghdeck.'", true)
	}
	return m, nil
}

func (m model) dir() string { return or(m.repo.Root, ".") }

func (m model) switchTab(i int) (tea.Model, tea.Cmd) {
	m.tab, m.typing = i, false
	m.say("", false)
	return m, m.refreshTab(i)
}

// checkoutCmd checks a pull request out in this checkout, asking first. It says why not when it cannot.
func checkoutCmd(r Repo, number int) (tea.Cmd, string) {
	if r.Root == "" {
		return nil, "no checkout here to check out into"
	}
	prompt := fmt.Sprintf("Check out #%d here, in %s?", number, r.Root)
	if dirty(r.Root) {
		prompt = fmt.Sprintf("You have uncommitted changes. Check out #%d anyway?", number)
	}
	return openPane(fmt.Sprintf("checkout #%d", number), r.Root,
		fmt.Sprintf("gh pr checkout %d -R %s", number, shellQuote(r.Slug)), false), prompt
}

// worktreeCmd checks a pull request out in a worktree beside this checkout and, in Herdr, opens it as a workspace:
// a place to run it or hand it to an agent without touching what you have checked out. An existing one is reused.
func worktreeCmd(r Repo, number int) tea.Cmd {
	if r.Root == "" {
		return func() tea.Msg { return actionMsg{err: errors.New("no checkout here to make a worktree from")} }
	}
	path := fmt.Sprintf("%s-pr-%d", r.Root, number)
	script := fmt.Sprintf(`set -e
p=%s
if [ ! -d "$p" ]; then
  git worktree add --detach "$p"
  cd "$p" && gh pr checkout %d -R %s
  cd - >/dev/null
fi
echo "worktree: $p"`, shellQuote(path), number, shellQuote(r.Slug))
	if inHerdr() {
		script += fmt.Sprintf("\n%s worktree open --cwd %s --path \"$p\" --focus >/dev/null && echo 'opened as a Herdr workspace'",
			shellQuote(herdrBin()), shellQuote(r.Root))
	}
	return viewAction(fmt.Sprintf("worktree for #%d", number), r.Root, script, false)
}

// itemPrompt asks an agent to take on a pull request or an issue. It names the item and points at gh for the rest
// rather than pasting its text: anyone can write an issue, and what they wrote should reach the agent as something to
// read, not as the instructions it is given.
func itemPrompt(repo string, it Item, d *Detail) string {
	if !it.IsPR {
		return fmt.Sprintf("Work on GitHub issue #%d in %s, %q. Read it and its comments with `gh issue view %d -R %s --comments`, "+
			"then make the change on a new branch and open a pull request that closes it.", it.Number, repo, it.Title, it.Number, repo)
	}
	p := fmt.Sprintf("Pull request #%d in %s, %q (branch %s).", it.Number, repo, it.Title, it.Head)
	var todo []string
	if d != nil {
		open := 0
		for _, t := range d.Threads {
			if !t.Resolved {
				open++
			}
		}
		if open > 0 {
			todo = append(todo, fmt.Sprintf("address the %d unresolved review thread(s) (read them with `gh api repos/%s/pulls/%d/comments`)", open, repo, it.Number))
		}
		var failed []string
		for _, c := range d.Checks {
			if checkRank(c) == 0 {
				failed = append(failed, c.Name)
			}
		}
		if len(failed) > 0 {
			todo = append(todo, fmt.Sprintf("fix the failing checks (%s; see `gh pr checks %d -R %s`)", strings.Join(failed, ", "), it.Number, repo))
		}
	} else {
		if it.Review == "CHANGES_REQUESTED" {
			todo = append(todo, "address the requested changes")
		}
		if checksFailed(it.Checks) {
			todo = append(todo, fmt.Sprintf("fix the failing checks (see `gh pr checks %d -R %s`)", it.Number, repo))
		}
	}
	if len(todo) == 0 {
		return p + fmt.Sprintf(" Review it: read `gh pr view %d -R %s --comments` and `gh pr diff %d -R %s`, and tell me what you would change.",
			it.Number, repo, it.Number, repo)
	}
	return p + " Check out its branch, " + strings.Join(todo, ", and ") + ". Push the fixes to the branch."
}

// ---- view ----

var (
	cAccent = lipgloss.AdaptiveColor{Light: "#5A3FD6", Dark: "#A08CFF"}
	cMuted  = lipgloss.AdaptiveColor{Light: "#6B6B76", Dark: "#8A8A96"}
	cGood   = lipgloss.AdaptiveColor{Light: "#18794E", Dark: "#4CC38A"}
	cWarn   = lipgloss.AdaptiveColor{Light: "#A35200", Dark: "#F5A524"}
	cBad    = lipgloss.AdaptiveColor{Light: "#C62A2F", Dark: "#FF6369"}
	cBorder = lipgloss.AdaptiveColor{Light: "#D4D4DA", Dark: "#3A3A44"}
	cMerged = lipgloss.AdaptiveColor{Light: "#8250DF", Dark: "#B88CFF"}
	cSelBg  = lipgloss.AdaptiveColor{Light: "#ECE9FB", Dark: "#2A2640"}

	sMuted  = lipgloss.NewStyle().Foreground(cMuted)
	sGood   = lipgloss.NewStyle().Foreground(cGood)
	sWarn   = lipgloss.NewStyle().Foreground(cWarn)
	sBad    = lipgloss.NewStyle().Foreground(cBad)
	sMerged = lipgloss.NewStyle().Foreground(cMerged)
	sBold   = lipgloss.NewStyle().Bold(true)
	sTitle  = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sTabOn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(cAccent).Padding(0, 1)
	sTabOff = lipgloss.NewStyle().Foreground(cMuted).Padding(0, 1)
	sRule   = lipgloss.NewStyle().Foreground(cBorder)
)

func (m model) View() string {
	if m.w == 0 {
		return "loading…"
	}
	toast := m.toast()
	if n := len(m.stack); n > 0 {
		if toast == "" {
			return m.stack[n-1].View(m.w, m.h)
		}
		return m.stack[n-1].View(m.w, m.h-1) + "\n" + toast
	}
	var head []string
	tabs := []string{sTitle.Render("ghdeck") + " "}
	for i, name := range tabNames {
		style := sTabOff
		if i == m.tab {
			style = sTabOn
		}
		label := fmt.Sprintf("%d %s", i+1, name)
		if i != tabRuns {
			if l, p := m.list(i); p != nil && p.val != nil {
				label += fmt.Sprintf(" %d", l.Total)
			}
		}
		tabs = append(tabs, style.Render(label))
	}
	head = append(head, truncStyledLine(lipgloss.JoinHorizontal(lipgloss.Top, tabs...), m.w))
	head = append(head, truncStyledLine(m.statusLine(), m.w))
	if m.authExpired() {
		head = append(head, sBad.Render(" ⚠ gh login expired. Press A to sign in, then r."))
	}
	if note := ghBudget(); note != "" {
		head = append(head, sWarn.Render(" ⚠ "+note))
	}
	head = append(head, m.filterLine())

	foot := m.footer()
	bodyH := max(1, m.h-len(head)-lipgloss.Height(foot))
	var body []string
	if m.tab == tabRuns {
		body = m.runRows(bodyH)
	} else {
		body = m.itemRows(bodyH)
	}
	for len(body) < bodyH {
		body = append(body, "")
	}
	return strings.Join(head, "\n") + "\n" + strings.Join(body, "\n") + "\n" + foot
}

// toast is the last action's result, shown for a few seconds over whatever view is on top.
func (m model) toast() string {
	if m.flash == "" || len(m.stack) == 0 || time.Since(m.flashAt) > 6*time.Second {
		return ""
	}
	style := sGood
	if m.isErr {
		style = sBad
	}
	return style.Render(" " + truncate(m.flash, m.w-2))
}

func (m model) statusLine() string {
	parts := []string{m.repo.Slug}
	s := m.status()
	if m.branch != "" {
		b := "on " + m.branch
		if pr := s.BranchPR; pr != nil {
			b += sMuted.Render(" → ") + fmt.Sprintf("#%d", pr.Number)
			if pr.Checks != "" {
				b += " " + checksIcon(pr.Checks)
			}
			if w := reviewWord(pr.Review); w != "" {
				b += " " + w
			}
			b += sMuted.Render(" · b")
		}
		parts = append(parts, b)
	}
	if n := len(s.ReviewFor); n > 0 {
		parts = append(parts, sWarn.Render(fmt.Sprintf("%d awaiting your review", n)))
	}
	line := " " + strings.Join(parts, sMuted.Render(" · "))
	if p := m.panels[kStatus]; p != nil && p.err != nil && !errors.Is(p.err, errAuth) {
		line += "  " + sBad.Render(truncate(errText(p.err), 60))
	}
	return line
}

func (m model) filterLine() string {
	if m.typing {
		return sWarn.Render(" search: ") + m.draft + "█" + sMuted.Render("  GitHub search syntax · enter search · esc cancel")
	}
	var what string
	var p *panel
	switch m.tab {
	case tabRuns:
		what = "all branches"
		if m.runsMine {
			what = "branch " + or(m.branch, "(none)")
		}
		p = m.panels[kRuns]
	default:
		fs := prFilters
		if m.tab == tabIssues {
			fs = issueFilters
		}
		what = fs[m.filter[m.tab]].name
		if s := m.search[m.tab]; s != "" {
			what = "search: " + s
		}
		_, p = m.list(m.tab)
	}
	line := " " + sBold.Render(what)
	if p != nil && p.loading {
		line += sMuted.Render(" ⟳")
	} else if p != nil && !p.at.IsZero() {
		line += sMuted.Render(" · " + age(p.at) + " ago")
	}
	return line
}

func (m model) itemRows(h int) []string {
	l, p := m.list(m.tab)
	var out []string
	if p != nil && p.err != nil {
		out = append(out, sBad.Render(" "+truncate(errText(p.err), m.w-2)))
		h--
	}
	if len(l.Items) == 0 {
		switch {
		case p == nil || p.loading && p.val == nil:
			out = append(out, sMuted.Render(" loading…"))
		case p.err == nil:
			out = append(out, sMuted.Render(" nothing here · f for another filter, / to search"))
		}
		return out
	}
	start := scrollStart(m.sel[m.tab], len(l.Items), h)
	for i := start; i < len(l.Items) && i < start+h; i++ {
		out = append(out, m.itemRow(l.Items[i], i == m.sel[m.tab]))
	}
	return out
}

// scrollStart keeps the cursor on screen, a little way from the bottom.
func scrollStart(sel, n, h int) int {
	if h <= 0 || n <= h {
		return 0
	}
	start := sel - h + max(1, h/4)
	return max(0, min(start, n-h))
}

func (m model) itemRow(it Item, on bool) string {
	cursor := "  "
	if on {
		cursor = sTitle.Render("▸ ")
	}
	num := sMuted.Render(pad(fmt.Sprintf("#%d", it.Number), 6))
	state := stateIcon(it)
	// Fixed-width columns on the right, so titles line up whatever each row has.
	var right []string
	if it.IsPR {
		right = append(right, checksIcon(it.Checks)+reviewIcon(it.Review))
		if m.w >= 90 {
			right = append(right, pad(sGood.Render(fmt.Sprintf("+%d", it.Additions))+sBad.Render(fmt.Sprintf("−%d", it.Deletions)), 11))
		}
	} else if m.w >= 90 {
		right = append(right, pad(sMuted.Render(truncate(strings.Join(it.Labels, ","), 18)), 18))
	}
	comments := ""
	if it.Comments > 0 {
		comments = sMuted.Render(fmt.Sprintf("💬%d", it.Comments))
	}
	right = append(right, pad(comments, 5))
	if m.w >= 70 {
		right = append(right, sMuted.Render(pad(truncate(it.Author, 14), 14)))
	}
	right = append(right, sMuted.Render(pad(age(it.Updated), 4)))
	tail := strings.Join(right, " ")
	title := it.Title
	style := lipgloss.NewStyle()
	if it.Draft {
		style = sMuted
		title = "draft · " + title
	}
	if on {
		style = style.Bold(true)
	}
	avail := m.w - lipgloss.Width(cursor+num+state) - lipgloss.Width(tail) - 3
	return cursor + num + state + " " + style.Render(pad(truncate(title, max(8, avail)), max(8, avail))) + " " + tail
}

func (m model) runRows(h int) []string {
	runs := m.runs()
	p := m.panels[kRuns]
	var out []string
	if p != nil && p.err != nil {
		out = append(out, sBad.Render(" "+truncate(errText(p.err), m.w-2)))
		h--
	}
	if len(runs) == 0 {
		if p == nil || p.loading && p.val == nil {
			return append(out, sMuted.Render(" loading…"))
		}
		if p.err == nil {
			return append(out, sMuted.Render(" no workflow runs"))
		}
		return out
	}
	sel := m.sel[tabRuns]
	start := scrollStart(sel, len(runs), h)
	for i := start; i < len(runs) && i < start+h; i++ {
		r := runs[i]
		cursor := "  "
		if i == sel {
			cursor = sTitle.Render("▸ ")
		}
		left := cursor + runIcon(r) + " " + sBold.Render(pad(truncate(r.Name, 16), 16)) + " "
		tail := sMuted.Render(pad(truncate(eventName(r.Event), 9), 9) + " " + pad(age(r.Created), 4))
		branchW := 18
		if m.w < 100 {
			branchW = 12
		}
		branch := sMuted.Render(pad(truncate(r.Branch, branchW), branchW)) + " "
		avail := max(8, m.w-lipgloss.Width(left+branch+tail)-2)
		out = append(out, left+branch+pad(truncate(r.Title, avail), avail)+" "+tail)
	}
	return out
}

func (m model) footer() string {
	if c := m.confirm; c != nil {
		return c.view()
	}
	var b strings.Builder
	if m.flash != "" && time.Since(m.flashAt) < 8*time.Second {
		style := sGood
		if m.isErr {
			style = sBad
		}
		b.WriteString(style.Render(" "+truncate(m.flash, m.w-2)) + "\n")
	}
	keys := [][2]string{{"tab", "list"}, {"↑↓ enter", "open"}}
	switch m.tab {
	case tabPRs:
		keys = append(keys, [2]string{"d", "diff"}, [2]string{"f", "filter"}, [2]string{"/", "search"})
	case tabIssues:
		keys = append(keys, [2]string{"f", "filter"}, [2]string{"/", "search"})
	case tabRuns:
		keys = append(keys, [2]string{"f", "this branch"})
	}
	if inHerdr() && m.tab != tabRuns {
		keys = append(keys, [2]string{"a", "agent"})
	}
	keys = append(keys, [2]string{"?", "more"}, [2]string{"q", "quit"})
	if m.help {
		where := "in a pane below"
		if !inHerdr() {
			where = "in this terminal"
		}
		b.WriteString(sMuted.Render(fmt.Sprintf(" Interactive commands (gh pr create, checkout) open %s.", where)) + "\n")
		keys = [][2]string{{"b", "your branch's PR"}, {"n", "new PR / issue"}, {"C", "check out PR"}, {"W", "PR in a worktree"},
			{"o", "open on GitHub"}, {"r", "refresh"}, {"A", "gh sign in"}, {"1-3", "jump to list"}, {"?", "back"}}
	}
	b.WriteString(keyHelp(keys, m.w))
	return b.String()
}

// truncStyledLine cuts a styled line at the screen edge.
func truncStyledLine(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

func stateIcon(it Item) string {
	switch {
	case it.State == "MERGED":
		return sMerged.Render("◆")
	case it.State == "CLOSED":
		return sBad.Render("●")
	case it.Draft:
		return sMuted.Render("○")
	}
	return sGood.Render("●")
}

func checksIcon(state string) string {
	switch state {
	case "SUCCESS":
		return sGood.Render("✓")
	case "FAILURE", "ERROR":
		return sBad.Render("✗")
	case "PENDING", "EXPECTED":
		return sWarn.Render("◐")
	}
	return " "
}

func reviewIcon(decision string) string {
	switch decision {
	case "APPROVED":
		return sGood.Render("✔")
	case "CHANGES_REQUESTED":
		return sBad.Render("±")
	case "REVIEW_REQUIRED":
		return sMuted.Render("•")
	}
	return " "
}

func reviewWord(decision string) string {
	switch decision {
	case "APPROVED":
		return sGood.Render("approved")
	case "CHANGES_REQUESTED":
		return sBad.Render("changes requested")
	case "REVIEW_REQUIRED":
		return sMuted.Render("needs review")
	}
	return ""
}

func runIcon(r Run) string {
	switch {
	case r.Status != "completed":
		return sWarn.Render("◐ " + pad(strings.ReplaceAll(r.Status, "_", " "), 11))
	case r.Conclusion == "success":
		return sGood.Render("✓ " + pad("success", 11))
	case r.Conclusion == "skipped" || r.Conclusion == "cancelled" || r.Conclusion == "neutral":
		return sMuted.Render("– " + pad(r.Conclusion, 11))
	default:
		return sBad.Render("✗ " + pad(r.Conclusion, 11))
	}
}

func eventName(e string) string {
	switch e {
	case "pull_request", "pull_request_target":
		return "PR"
	case "workflow_dispatch":
		return "by hand"
	case "workflow_run":
		return "after run"
	case "schedule":
		return "scheduled"
	case "issue_comment":
		return "comment"
	}
	return strings.ReplaceAll(e, "_", " ")
}
