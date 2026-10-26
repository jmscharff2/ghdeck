package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// "Work on this" hands a pull request to an agent in its own worktree, in one key: ghdeck checks the branch out beside
// your checkout, opens that as a Herdr workspace, starts an agent there (or reuses the one already in it) with a prompt
// naming what is left to do, and then watches the pull request, so you hear when the agent pushes and how the checks
// came out. Your own checkout is never touched.

// agentKinds are the agents ghdeck can start, the first one installed winning. Each takes its first prompt from
// Herdr's agent prompt once it is ready.
var agentKinds = []string{"claude", "codex", "gemini", "opencode", "amp", "cursor", "copilot"}

func installedAgent() string {
	for _, k := range agentKinds {
		if _, err := exec.LookPath(k); err == nil {
			return k
		}
	}
	return ""
}

// workStartedMsg says an agent took a pull request, and ghdeck should watch it.
type workStartedMsg struct {
	number    int
	sha       string
	checks    string
	pane      string
	workspace string
	reused    bool
	err       error
}

// workPicker shows what will happen and the prompt the agent gets, which e edits, before anything starts.
func workPicker(r Repo, it Item, d *Detail) *picker {
	prompt := workPrompt(r.Slug, it, d)
	kind := or(installedAgent(), "an agent")
	p := newPicker(fmt.Sprintf("Work on #%d", it.Number), nil)
	p.note = func(w int) []string {
		lines := []string{
			sMuted.Render(fmt.Sprintf(" Checks out %s in %s-pr-%d, opens it as a Herdr workspace, and gives this to %s there:", or(it.Head, "its branch"), r.Root, it.Number, kind)),
		}
		for _, l := range wrapText(prompt, w-4) {
			lines = append(lines, "   "+l)
		}
		return append(lines, "", sMuted.Render(" Then ghdeck watches it, and tells you when the agent pushes and how the checks go."))
	}
	p.opts = []option{{label: "Start", detail: "your checkout stays as it is", run: func() tea.Cmd { return workCmd(r, it, d, prompt) }}}
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
	return p
}

// workCmd does the whole hand-over off the UI. It needs Herdr and a checkout.
func workCmd(r Repo, it Item, d *Detail, prompt string) tea.Cmd {
	return func() tea.Msg {
		fail := func(err error) tea.Msg { return workStartedMsg{number: it.Number, err: err} }
		if !inHerdr() {
			return fail(errors.New("working on a pull request needs Herdr, to open its worktree and agent"))
		}
		if r.Root == "" {
			return fail(errors.New("no checkout here to make a worktree from"))
		}
		path := fmt.Sprintf("%s-pr-%d", r.Root, it.Number)
		sha := it.HeadSHA
		if d != nil {
			sha = or(d.HeadSHA, sha)
		}
		script := worktreeScript(path, it.Number, r.Slug, sha)
		if out, err := runIn(r.Root, "sh", "-c", script); err != nil {
			return fail(fmt.Errorf("making the worktree at %s: %w %s", path, err, strings.TrimSpace(string(out))))
		}
		out, err := run(herdrBin(), "worktree", "open", "--cwd", r.Root, "--path", path,
			"--label", fmt.Sprintf("#%d %s", it.Number, truncate(it.Title, 30)), "--focus")
		if err != nil {
			return fail(fmt.Errorf("herdr worktree open: %w", err))
		}
		var opened struct {
			Result struct {
				Workspace struct {
					ID string `json:"workspace_id"`
				} `json:"workspace"`
				Already bool `json:"already_open"`
			} `json:"result"`
		}
		if json.Unmarshal(out, &opened) != nil || opened.Result.Workspace.ID == "" {
			return fail(errors.New("herdr worktree open: unexpected reply"))
		}
		ws := opened.Result.Workspace.ID
		msg := workStartedMsg{number: it.Number, sha: it.HeadSHA, checks: it.Checks, workspace: ws}
		if d != nil {
			msg.sha = or(d.HeadSHA, msg.sha)
		}
		// An agent already working in that workspace takes the prompt; otherwise one starts in its first pane.
		if agents, err := listAgents(r); err == nil {
			for _, a := range agents {
				if within(a.Cwd, path) {
					if _, err := run(herdrBin(), "agent", "prompt", a.Pane, prompt); err != nil {
						return fail(fmt.Errorf("herdr agent prompt: %w", err))
					}
					msg.pane, msg.reused = a.Pane, true
					return msg
				}
			}
		}
		kind := installedAgent()
		if kind == "" {
			return fail(fmt.Errorf("no agent to start: install one of %s, or start one in the new workspace", strings.Join(agentKinds, ", ")))
		}
		pane, err := workspacePane(ws)
		if err != nil {
			return fail(err)
		}
		if _, err := run(herdrBin(), "agent", "start", fmt.Sprintf("pr-%d", it.Number), "--kind", kind, "--pane", pane); err != nil {
			return fail(fmt.Errorf("herdr agent start: %w", err))
		}
		if _, err := run(herdrBin(), "agent", "prompt", pane, prompt); err != nil {
			return fail(fmt.Errorf("herdr agent prompt: %w", err))
		}
		msg.pane = pane
		return msg
	}
}

// workspacePane finds a workspace's first pane, for the agent to start in.
func workspacePane(ws string) (string, error) {
	out, err := run(herdrBin(), "pane", "list", "--workspace", ws)
	if err != nil {
		return "", fmt.Errorf("herdr pane list: %w", err)
	}
	var resp struct {
		Result struct {
			Panes []struct {
				ID        string `json:"pane_id"`
				Workspace string `json:"workspace_id"`
			} `json:"panes"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &resp) != nil {
		return "", errors.New("herdr pane list: unexpected reply")
	}
	for _, p := range resp.Result.Panes {
		if p.Workspace == ws || p.Workspace == "" {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("workspace %s has no pane for the agent", ws)
}

// workPrompt is itemPrompt for an agent already in the pull request's worktree: the branch is checked out, so it
// can get straight to what is left, and push.
func workPrompt(repo string, it Item, d *Detail) string {
	p := itemPrompt(repo, it, d)
	p = strings.Replace(p, " Check out its branch, ", " Its branch is checked out in this directory, a worktree of its own: ", 1)
	return p + " When you push, I'll hear about it and how the checks go."
}

// watch is a pull request handed to an agent, and what was last seen of it.
type watch struct {
	sha, checks, pane string
}

// announceWatched tells you when a watched pull request gets new commits, and how its checks end, until it closes.
func (m *model) announceWatched(s Status) tea.Cmd {
	var cmds []tea.Cmd
	for n, w := range m.watching {
		it, ok := s.Watched[n]
		if !ok {
			continue
		}
		what := fmt.Sprintf("#%d %s", n, it.Title)
		if it.State != "OPEN" {
			cmds = append(cmds, notify("No longer watching", fmt.Sprintf("%s is %s", what, strings.ToLower(it.State)), true))
			delete(m.watching, n)
			continue
		}
		pushed := it.HeadSHA != "" && w.sha != "" && it.HeadSHA != w.sha
		if pushed {
			cmds = append(cmds, notify("Agent pushed", fmt.Sprintf("%s · %s · checks running · R asks reviewers again", what, short(it.HeadSHA)), false))
		}
		// How the checks ended, once for each commit: news when they change, or when there is a new commit.
		switch {
		case checksFailed(it.Checks) && (pushed || !checksFailed(w.checks)):
			cmds = append(cmds, notify("Checks failed", what, false))
		case it.Checks == "SUCCESS" && (pushed || w.checks != "SUCCESS"):
			cmds = append(cmds, notify("Checks passed", what, true))
		}
		w.sha, w.checks = or(it.HeadSHA, w.sha), it.Checks
	}
	return tea.Batch(cmds...)
}
