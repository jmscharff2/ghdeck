// ghdeck is a terminal deck for the GitHub repository you are working in: its pull requests, issues and Actions runs.
// Review a diff line by line, comment, approve, merge, read a failed job's log, and, inside Herdr, hand any of it to a
// coding agent in another pane.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

var version = "dev"

const usage = `ghdeck: pull requests, issues and Actions for the repository you are in

Usage:
  ghdeck [-R owner/name]    open the deck for this checkout's GitHub repo, or the one named
  ghdeck -branch            open straight onto the checked-out branch's pull request
  ghdeck version

ghdeck uses the gh CLI's login (gh auth login) and reads github.com, or $GH_HOST.
`

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("ghdeck", version)
		return
	}
	fs := flag.NewFlagSet("ghdeck", flag.ExitOnError)
	repoFlag := fs.String("R", "", "repository as owner/name")
	branchFlag := fs.Bool("branch", false, "open the checked-out branch's pull request")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	_ = fs.Parse(os.Args[1:])

	r, err := findRepo(".", *repoFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghdeck:", err)
		os.Exit(1)
	}
	m := newModel(r)
	m.openBranch = *branchFlag
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ghdeck:", err)
		os.Exit(1)
	}
}
