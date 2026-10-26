package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func inHerdr() bool { return os.Getenv("HERDR_ENV") == "1" && os.Getenv("HERDR_PANE_ID") != "" }

func herdrBin() string { return or(os.Getenv("HERDR_BIN_PATH"), "herdr") }

// execMsg asks Update to hand the terminal to command, for when there is no Herdr to open a pane in.
type execMsg struct {
	label, dir, command string
}

// openPane runs command in a new Herdr pane below this one, or in this terminal outside Herdr.
// focus moves the cursor into the new pane, for interactive things like `gh pr create`.
func openPane(label, dir, command string, focus bool) tea.Cmd {
	return func() tea.Msg {
		if !inHerdr() {
			return execMsg{label: label, dir: dir, command: command}
		}
		args := []string{"pane", "split", "--current", "--direction", "down", "--cwd", dir}
		if !focus {
			args = append(args, "--no-focus")
		}
		out, err := run(herdrBin(), args...)
		if err != nil {
			return actionMsg{err: fmt.Errorf("herdr split: %w", err)}
		}
		var resp struct {
			Result struct {
				Pane struct {
					ID string `json:"pane_id"`
				} `json:"pane"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &resp); err != nil || resp.Result.Pane.ID == "" {
			return actionMsg{err: fmt.Errorf("herdr split: unexpected reply %q", out)}
		}
		id := resp.Result.Pane.ID
		if _, err := run(herdrBin(), "pane", "run", id, command); err != nil {
			return actionMsg{err: fmt.Errorf("herdr run: %w", err)}
		}
		return actionMsg{text: fmt.Sprintf("%s → pane %s", label, id), refresh: true}
	}
}

func execHere(m execMsg) tea.Cmd {
	c := exec.Command("sh", "-c", m.command)
	c.Dir = m.dir
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return actionMsg{err: fmt.Errorf("%s: %w", m.label, err), refresh: true}
		}
		return actionMsg{text: m.label + " finished", refresh: true}
	})
}

// notify shows a Herdr notification, or rings the terminal bell outside Herdr.
func notify(title, body string, good bool) tea.Cmd {
	return func() tea.Msg {
		if inHerdr() {
			sound := "request"
			if good {
				sound = "done"
			}
			_, _ = run(herdrBin(), "notification", "show", title, "--body", body, "--sound", sound)
		} else {
			fmt.Print("\a")
		}
		return actionMsg{text: title + ": " + body}
	}
}

// ---- handing work to an agent ----

// agent is a coding agent Herdr is running in some pane.
type agent struct {
	Kind      string `json:"agent"`
	Status    string `json:"agent_status"`
	Pane      string `json:"pane_id"`
	Workspace string `json:"workspace_id"`
	Cwd       string `json:"foreground_cwd"`
	Title     string `json:"terminal_title_stripped"`
	// here is set for an agent working in this repo (its checkout or a worktree beside it).
	here bool
}

// listAgents returns Herdr's agents, the ones working in this repo first, then this workspace's.
func listAgents(r Repo) ([]agent, error) {
	out, err := run(herdrBin(), "agent", "list")
	if err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", err)
	}
	var resp struct {
		Result struct {
			Agents []agent `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("herdr agent list: unexpected reply")
	}
	ws := os.Getenv("HERDR_WORKSPACE_ID")
	var agents []agent
	for _, a := range resp.Result.Agents {
		if a.Pane == os.Getenv("HERDR_PANE_ID") {
			continue
		}
		a.Kind, a.Status, a.Title, a.Cwd = clean(a.Kind), clean(a.Status), clean(a.Title), clean(a.Cwd)
		a.here = r.Root != "" && (within(a.Cwd, r.Root) || sameRepoDir(a.Cwd, r))
		agents = append(agents, a)
	}
	sort.SliceStable(agents, func(i, j int) bool {
		rank := func(a agent) int {
			switch {
			case a.here:
				return 0
			case a.Workspace == ws:
				return 1
			}
			return 2
		}
		return rank(agents[i]) < rank(agents[j])
	})
	return agents, nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// agentsByBranch finds which branch each of this repo's agents has checked out, so a pull request can show the agent
// on it. Only local git is asked; when two agents share a branch, the one working wins.
func agentsByBranch(r Repo) map[string]agent {
	agents, err := listAgents(r)
	if err != nil {
		return nil
	}
	rank := map[string]int{"working": 0, "blocked": 1, "idle": 2, "done": 3}
	out := map[string]agent{}
	for _, a := range agents {
		if !a.here || a.Cwd == "" {
			continue
		}
		b := currentBranch(a.Cwd)
		if b == "" {
			continue
		}
		if have, ok := out[b]; !ok || rank[a.Status] < rank[have.Status] {
			out[b] = a
		}
	}
	return out
}

// agentMark is how a list shows the agent on a pull request's branch: its kind, coloured by what it is doing.
func agentMark(a agent) string {
	style := sMuted
	switch a.Status {
	case "working":
		style = sWarn
	case "blocked":
		style = sBad
	case "idle", "done":
		style = sGood
	}
	return style.Render("◆ " + or(a.Kind, "agent") + " ")
}

// sameRepoDir recognizes a worktree of the same repo: git reports the same common directory for it.
func sameRepoDir(dir string, r Repo) bool {
	if r.CommonDir == "" {
		return false
	}
	out, err := runIn(dir, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	return err == nil && strings.TrimSpace(string(out)) == r.CommonDir
}

// promptAgent submits a prompt to an agent, as if typed into it.
func promptAgent(a agent, prompt string) tea.Cmd {
	return func() tea.Msg {
		if _, err := run(herdrBin(), "agent", "prompt", a.Pane, prompt); err != nil {
			return actionMsg{err: fmt.Errorf("herdr agent prompt: %w", err)}
		}
		return actionMsg{text: fmt.Sprintf("sent to %s in pane %s", or(a.Kind, "agent"), a.Pane)}
	}
}

// newAgentCommand starts a fresh agent with the prompt, for when none is running. Claude Code takes its first prompt
// as an argument.
func newAgentCommand(prompt string) (string, bool) {
	if _, err := exec.LookPath("claude"); err != nil {
		return "", false
	}
	return "claude " + shellQuote(prompt), true
}
