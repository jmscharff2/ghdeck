package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// composer writes some text: a comment, a reply, a review's summary, a prompt for an agent. ctrl+s sends it, ctrl+e
// finishes it in $EDITOR, esc asks before throwing it away.
type composer struct {
	id      int
	title   string
	context []string // shown above the text: the line being commented on, say
	ta      textarea.Model
	// send does the work, off the UI, when there is a request to make; done runs on the UI after it succeeds (or at
	// once when send is nil) and says what happens next.
	send       func(body string) error
	done       func(body string) tea.Cmd
	allowEmpty bool
	sending    bool
	err        error
	discard    bool // esc pressed once with text written
}

type composeSentMsg struct {
	id   int
	body string
	err  error
}

func (m composeSentMsg) viewID() int { return m.id }

type composeEditedMsg struct {
	id   int
	text string
	err  error
}

func (m composeEditedMsg) viewID() int { return m.id }

func newComposer(title string, context []string, initial string) *composer {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.Prompt = "│ "
	ta.Placeholder = "Markdown welcome"
	ta.SetValue(initial)
	ta.Focus()
	return &composer{id: nextViewID(), title: title, context: context, ta: ta}
}

func (c *composer) ID() int       { return c.id }
func (c *composer) Init() tea.Cmd { return textarea.Blink }
func (c *composer) Close()        {}

func (c *composer) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case composeSentMsg:
		c.sending = false
		if msg.err != nil {
			c.err = msg.err
			return true, nil
		}
		if c.done != nil {
			return false, c.done(msg.body)
		}
		return false, nil
	case composeEditedMsg:
		if msg.err != nil {
			c.err = msg.err
		} else {
			c.ta.SetValue(strings.TrimRight(msg.text, "\n"))
		}
		return true, nil
	case tea.KeyMsg:
		if c.sending {
			return true, nil
		}
		switch msg.String() {
		case "ctrl+s":
			body := strings.TrimSpace(c.ta.Value())
			if body == "" && !c.allowEmpty {
				c.err = fmt.Errorf("write something first, or esc to cancel")
				return true, nil
			}
			if c.send == nil {
				if c.done != nil {
					return false, c.done(body)
				}
				return false, nil
			}
			c.sending, c.err = true, nil
			id, send := c.id, c.send
			return true, func() tea.Msg { return composeSentMsg{id: id, body: body, err: send(body)} }
		case "ctrl+e":
			return true, c.editExternally()
		case "esc":
			if strings.TrimSpace(c.ta.Value()) != "" && !c.discard {
				c.discard = true
				return true, nil
			}
			return false, nil
		}
		c.discard = false
	}
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	return true, cmd
}

// editExternally hands the text to $EDITOR (or vi) and takes back whatever is saved.
func (c *composer) editExternally() tea.Cmd {
	f, err := os.CreateTemp("", "ghdeck-*.md")
	if err != nil {
		c.err = err
		return nil
	}
	path := f.Name()
	_, _ = f.WriteString(c.ta.Value())
	f.Close()
	editor := or(os.Getenv("VISUAL"), or(os.Getenv("EDITOR"), "vi"))
	id := c.id
	cmd := exec.Command("sh", "-c", editor+" "+shellQuote(path))
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.Remove(path)
		if err != nil {
			return composeEditedMsg{id: id, err: fmt.Errorf("%s: %w", editor, err)}
		}
		b, err := os.ReadFile(path)
		return composeEditedMsg{id: id, text: string(b), err: err}
	})
}

func (c *composer) View(w, h int) string {
	var lines []string
	lines = append(lines, sTitle.Render(truncate(c.title, w-2)))
	for _, l := range c.context {
		lines = append(lines, truncStyledLine(l, w))
	}
	lines = append(lines, "")
	help := keyHelp([][2]string{{"ctrl+s", "send"}, {"ctrl+e", "$EDITOR"}, {"esc", "cancel"}}, w)
	c.ta.SetWidth(max(10, w-1))
	c.ta.SetHeight(max(3, h-len(lines)-1-lipgloss.Height(help)))
	body := c.ta.View()
	status := ""
	switch {
	case c.sending:
		status = sWarn.Render(" sending…")
	case c.err != nil:
		status = sBad.Render(" " + truncate(errText(c.err), w-2))
	case c.discard:
		status = sWarn.Render(" esc again to throw this away")
	}
	return strings.Join(lines, "\n") + "\n" + body + "\n" + status + "\n" + help
}

// ---- picker ----

// option is one choice in a picker. run says what choosing it does.
type option struct {
	label  string
	detail string
	match  string // what typing filters on, in a filterable picker: the label when empty
	run    func() tea.Cmd
	on     bool // starts ticked, in a multi picker
	hidden bool // listed only once typing finds it
}

type pickerLoadedMsg struct {
	id   int
	opts []option
	err  error
}

func (m pickerLoadedMsg) viewID() int { return m.id }

// picker asks you to choose one of a few things: a review's verdict, a merge method, an agent.
type picker struct {
	id      int
	title   string
	note    func(w int) []string // shown above the choices, redrawn to the width
	opts    []option
	sel     int
	load    func() ([]option, error) // fills opts when the picker opens
	loading bool
	err     error
	keys    [][2]string // extra keys for the help line
	onKey   func(key string) (bool, tea.Cmd)
	// filterable pickers take typing as a filter, for long lists like a repository's branches.
	filterable bool
	filter     string
	extra      func(filter string) []option // offered after the matches, when none is exactly what was typed; not while loading
	// multi pickers tick several options with space; enter hands done every ticked option's match (or label).
	multi  bool
	ticked map[string]bool
	done   func(ticked []string) tea.Cmd
	// skipEmpty closes the picker when it loads nothing to choose: an offer, made unasked, that does not apply.
	skipEmpty bool
}

func newPicker(title string, opts []option) *picker {
	return &picker{id: nextViewID(), title: title, opts: opts}
}

// newMultiPicker ticks several options, starting with those in ticked, and hands done the ones ticked at enter.
func newMultiPicker(title string, ticked []string, done func([]string) tea.Cmd) *picker {
	p := newPicker(title, nil)
	p.multi, p.filterable, p.done, p.ticked = true, true, done, map[string]bool{}
	for _, t := range ticked {
		p.ticked[t] = true
	}
	return p
}

func (p *picker) ID() int { return p.id }
func (p *picker) Close()  {}

func (p *picker) Init() tea.Cmd {
	if p.load == nil {
		return nil
	}
	p.loading = true
	id, load := p.id, p.load
	return func() tea.Msg {
		opts, err := load()
		return pickerLoadedMsg{id: id, opts: opts, err: err}
	}
}

func (p *picker) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case pickerLoadedMsg:
		p.loading, p.opts, p.err = false, msg.opts, msg.err
		if p.skipEmpty && msg.err == nil && len(msg.opts) == 0 {
			return false, nil
		}
		for _, o := range p.opts {
			if o.on && p.ticked != nil {
				p.ticked[or(o.match, o.label)] = true
			}
		}
	case tea.KeyMsg:
		shown := p.shown()
		key := msg.String()
		if p.filterable {
			switch {
			case key == "esc" && p.filter != "":
				p.filter, p.sel = "", 0
				return true, nil
			case key == "backspace":
				if r := []rune(p.filter); len(r) > 0 {
					p.filter, p.sel = string(r[:len(r)-1]), 0
				}
				return true, nil
			case msg.Type == tea.KeyRunes:
				p.filter, p.sel = p.filter+string(msg.Runes), 0
				return true, nil
			case p.multi && key == " ":
				if p.sel < len(shown) {
					k := or(shown[p.sel].match, shown[p.sel].label)
					p.ticked[k] = !p.ticked[k]
				}
				return true, nil
			case key == "j" || key == "k" || key == "q":
				return true, nil // letters filter here; arrows move
			}
		}
		switch key {
		case "esc", "q":
			return false, nil
		case "up", "k":
			p.sel = max(0, p.sel-1)
		case "down", "j":
			p.sel = max(0, min(len(shown)-1, p.sel+1))
		case "enter":
			if p.multi {
				var out []string
				for _, o := range p.opts { // in the order offered
					if k := or(o.match, o.label); p.ticked[k] {
						out = append(out, k)
					}
				}
				if p.done == nil {
					return false, nil
				}
				return false, p.done(out)
			}
			if p.sel < len(shown) {
				if run := shown[p.sel].run; run != nil {
					return false, run()
				}
				return false, nil
			}
		default:
			if p.onKey != nil {
				return p.onKey(msg.String())
			}
		}
	}
	return true, nil
}

// shown is the options that match the filter.
func (p *picker) shown() []option {
	if p.filter == "" {
		var out []option
		for _, o := range p.opts {
			if !o.hidden {
				out = append(out, o)
			}
		}
		return out
	}
	var out []option
	f, exact := strings.ToLower(p.filter), false
	for _, o := range p.opts {
		m := or(o.match, o.label)
		if strings.Contains(strings.ToLower(m), f) {
			out = append(out, o)
			exact = exact || m == p.filter
		}
	}
	if p.extra != nil && !exact && !p.loading {
		out = append(out, p.extra(p.filter)...)
	}
	return out
}

func (p *picker) View(w, h int) string {
	lines := []string{sTitle.Render(truncate(p.title, w-2)), ""}
	if p.note != nil {
		lines = append(lines, p.note(w)...)
		lines = append(lines, "")
	}
	if p.filterable {
		if p.filter == "" {
			lines = append(lines, sMuted.Render(" type to filter"))
		} else {
			lines = append(lines, " "+sBold.Render(p.filter)+sMuted.Render("▏"))
		}
	}
	shown := p.shown()
	switch {
	case p.loading:
		lines = append(lines, sMuted.Render(" loading…"))
	case p.err != nil:
		lines = append(lines, sBad.Render(" "+truncate(errText(p.err), w-2)))
	case len(shown) == 0 && p.filter != "":
		lines = append(lines, sMuted.Render(" nothing matches"))
	case len(shown) == 0:
		lines = append(lines, sMuted.Render(" nothing to choose from"))
	}
	// The choices scroll, keeping the chosen one in sight.
	var rows []string
	selRow := 0
	for i, o := range shown {
		cursor := "  "
		label := o.label
		if p.multi {
			box := sMuted.Render("☐ ")
			if p.ticked[or(o.match, o.label)] {
				box = sGood.Render("☑ ")
			}
			label = box + label
		}
		if i == p.sel {
			cursor = sTitle.Render("▸ ")
			label = sBold.Render(label)
			selRow = len(rows)
		}
		rows = append(rows, truncStyledLine(cursor+label, w))
		if o.detail != "" {
			rows = append(rows, truncStyledLine("    "+sMuted.Render(o.detail), w))
		}
	}
	keys := [][2]string{{"↑↓", "choose"}, {"enter", "go"}}
	if p.multi {
		keys = [][2]string{{"↑↓", "choose"}, {"space", "tick"}, {"enter", "apply"}}
	}
	keys = append(keys, p.keys...)
	keys = append(keys, [2]string{"esc", "cancel"})
	help := keyHelp(keys, w)
	bodyH := h - lipgloss.Height(help)
	if room := bodyH - len(lines); room > 0 && len(rows) > room {
		start := scrollStart(selRow, len(rows), room)
		if selRow+1 < len(rows) && selRow+1 >= start+room {
			start++ // keep the chosen option's detail line with it
		}
		rows = rows[start:min(len(rows), start+room)]
	}
	lines = append(lines, rows...)
	if len(lines) > bodyH {
		lines = lines[:bodyH]
	}
	for len(lines) < bodyH {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n") + "\n" + help
}

// newAgentPicker asks which Herdr agent should take a prompt, showing the prompt first: e edits it before it goes.
func newAgentPicker(r Repo, prompt string) *picker {
	p := newPicker("Hand to an agent", nil)
	p.note = func(w int) []string {
		lines := []string{sMuted.Render(" This prompt goes to the agent as if you typed it:")}
		for _, l := range wrapText(prompt, w-4) {
			lines = append(lines, "   "+l)
		}
		return lines
	}
	p.keys = [][2]string{{"e", "edit prompt"}}
	p.onKey = func(key string) (bool, tea.Cmd) {
		if key != "e" {
			return true, nil
		}
		c := newComposer("Edit the prompt", nil, prompt)
		c.done = func(body string) tea.Cmd {
			prompt = body
			return nil
		}
		return true, pushView(c)
	}
	p.load = func() ([]option, error) {
		agents, err := listAgents(r)
		if err != nil {
			return nil, err
		}
		var opts []option
		home, _ := os.UserHomeDir()
		for _, a := range agents {
			a := a
			where := a.Cwd
			if home != "" && strings.HasPrefix(where, home) {
				where = "~" + strings.TrimPrefix(where, home)
			}
			label := fmt.Sprintf("%s  %s", or(a.Kind, "agent"), agentStatus(a.Status))
			if a.here {
				label += sGood.Render("  this repo")
			}
			opts = append(opts, option{
				label:  label,
				detail: fmt.Sprintf("pane %s · %s · %s", a.Pane, truncate(a.Title, 40), where),
				run:    func() tea.Cmd { return promptAgent(a, prompt) },
			})
		}
		if _, ok := newAgentCommand(""); ok {
			opts = append(opts, option{
				label:  "new Claude Code agent",
				detail: "in a pane below, in " + or(r.Root, "this directory"),
				run: func() tea.Cmd {
					cmd, _ := newAgentCommand(prompt)
					return openPane("new agent", or(r.Root, "."), cmd, true)
				},
			})
		}
		return opts, nil
	}
	return p
}

func agentStatus(s string) string {
	switch s {
	case "idle", "done":
		return sGood.Render(s)
	case "working":
		return sWarn.Render(s)
	case "blocked":
		return sBad.Render(s + " (waiting on you)")
	}
	return sMuted.Render(s)
}
