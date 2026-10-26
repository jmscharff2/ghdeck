package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// In Herdr, ghdeck puts each workspace's pull request in the sidebar, as the $gh_pr token: "#482 ✓ approved" under a
// workspace whose checkout is on that pull request's branch. It covers every workspace working in this repository
// (your checkout and its worktrees), from what ghdeck already fetched: nothing more is asked of GitHub. Tokens carry a
// time to live, so they go when ghdeck does. Sidebar rows show them once the config names them; see the README.

const sidebarToken = "gh_pr"

// sidebarSeen remembers what each workspace was told, so unchanged values are not reported every refresh.
var sidebarSeen = struct {
	sync.Mutex
	value map[string]string
	at    map[string]time.Time
}{value: map[string]string{}, at: map[string]time.Time{}}

// sidebarText is a pull request as the sidebar shows it: its number, then how it stands.
func sidebarText(it Item) string {
	how := "open"
	switch {
	case it.Draft:
		how = "draft"
	case checksFailed(it.Checks):
		how = "✗ checks"
	case it.Checks == "PENDING" || it.Checks == "EXPECTED":
		how = "◐ checks"
	case it.Review == "APPROVED":
		how = "✓ approved"
	case it.Review == "CHANGES_REQUESTED":
		how = "changes requested"
	case it.Checks == "SUCCESS":
		how = "✓ checks"
	}
	return fmt.Sprintf("#%d %s", it.Number, how)
}

// reportSidebar tells Herdr each of this repo's workspaces its pull request, by the branch its panes are on. prs maps
// branch to pull request; ttl is how long a value lasts unless ghdeck reports it again.
func reportSidebar(r Repo, prs map[string]Item, ttl time.Duration) {
	if !inHerdr() || r.Root == "" {
		return
	}
	out, err := run(herdrBin(), "pane", "list")
	if err != nil {
		return
	}
	var resp struct {
		Result struct {
			Panes []struct {
				Workspace string `json:"workspace_id"`
				Cwd       string `json:"foreground_cwd"`
				Cwd2      string `json:"cwd"`
			} `json:"panes"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &resp) != nil {
		return
	}
	ours := map[string]bool{} // dir is in this repository, asked once each
	branchOf := map[string]string{}
	for _, p := range resp.Result.Panes {
		dir := or(p.Cwd, p.Cwd2)
		if dir == "" || branchOf[p.Workspace] != "" {
			continue
		}
		in, asked := ours[dir]
		if !asked {
			in = within(dir, r.Root) || sameRepoDir(dir, r)
			ours[dir] = in
		}
		if in {
			if b := currentBranch(dir); b != "" {
				branchOf[p.Workspace] = b
			}
		}
	}
	ttlMS := strconv.FormatInt(min(ttl, 24*time.Hour).Milliseconds(), 10)
	for ws, branch := range branchOf {
		args := []string{"workspace", "report-metadata", ws, "--source", "ghdeck", "--ttl-ms", ttlMS}
		value := ""
		if it, ok := prs[branch]; ok {
			value = sidebarText(it)
			args = append(args, "--token", sidebarToken+"="+value)
		} else {
			args = append(args, "--clear-token", sidebarToken)
		}
		sidebarSeen.Lock()
		// Clear only what this deck set: a workspace with panes in another repository may have that deck's value.
		same := sidebarSeen.value[ws] == value && (value == "" || time.Since(sidebarSeen.at[ws]) < ttl/2)
		if !same {
			sidebarSeen.value[ws], sidebarSeen.at[ws] = value, time.Now()
		}
		sidebarSeen.Unlock()
		if !same {
			_, _ = run(herdrBin(), args...)
		}
	}
}
