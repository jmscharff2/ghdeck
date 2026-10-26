package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// confirmation asks before something that changes things on GitHub: a merge, closing an issue, re-running a run,
// checking out a branch over your work. It always takes a typed word, never a single key, so a stray keystroke cannot
// merge a pull request; and pasted text does not count, so neither can text pasted into the pane by mistake or by
// another program (Herdr's agent prompts arrive as a paste).
type confirmation struct {
	prompt  string
	require string // the word to type
	typed   string
	cmd     tea.Cmd
	hint    string
}

func newConfirmation(prompt string, cmd tea.Cmd) *confirmation {
	return &confirmation{prompt: prompt, require: "yes", cmd: cmd}
}

// key handles one key. done means the question is over; ok means it was answered yes.
func (c *confirmation) key(msg tea.KeyMsg) (done, ok bool) {
	if msg.Paste {
		c.typed, c.hint = "", "pasted text does not count: type it"
		return false, false
	}
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		return true, false
	case tea.KeyEnter:
		if c.typed == c.require {
			return true, true
		}
		c.typed, c.hint = "", "type "+c.require+" exactly, or esc to cancel"
	case tea.KeyBackspace:
		if r := []rune(c.typed); len(r) > 0 {
			c.typed = string(r[:len(r)-1])
		}
	case tea.KeyRunes, tea.KeySpace:
		c.typed += string(msg.Runes)
		c.hint = ""
	}
	return false, false
}

func (c *confirmation) view() string {
	s := sWarn.Render(" "+c.prompt) + fmt.Sprintf(" Type %s and press enter: ", sBold.Render(c.require)) + c.typed + "█"
	if c.hint != "" {
		s += sMuted.Render("  " + c.hint)
	}
	return s
}
