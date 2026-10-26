# Changelog

## 0.2.0

**Reviewing**
- Resolve threads (`z`); resolved threads fold to a line. Mark files viewed (`V`). See only what changed since your
  last review (`S`).
- Highlighted diffs, and side by side in wide panes (`s`). Big files stay quick to move through.
- Comment on several lines (`m`, then `c`). Suggest changes (`C`), and apply others' suggestions to your checkout (`a`).
- Edit or delete your comments, in the conversation (`E`) and in the diff (`e`, `x`).

**Pull requests and issues**
- Create a pull request from any branch into any other (`n`), with the likeliest target guessed from the repository's
  history and promotions described as release notes.
- Reviewers (`R`), including asking past reviewers again; labels (`L`); edit the title and description (`e`).
- Update a branch from its base (`u`). Auto-merge, and the merge queue, from the merge picker (`m`).
- Saved searches as filters, from `~/.config/ghdeck/config.json` or a repository's `.ghdeck.json`; `F` picks one.
- An inbox of notifications from every repository (tab `4`).
- The mouse: click tabs and rows, scroll with the wheel.

**Herdr**
- Work on a pull request with an agent in its own worktree (`w`), and hear when it pushes and how the checks go.
- See which agent is on which pull request, and switch to it (`J`).
- Each workspace's pull request in the sidebar, as `$gh_pr`.
- GitHub links clicked in any pane open in ghdeck, GitHub Enterprise included.
- Decks open at the same time share what they fetch from GitHub.

**Everything else**
- Polling slows while ghdeck sits idle. `ghdeck -open` opens a pull request, issue or run by number or link.
- CI on every pull request; tagged releases publish binaries.

## 0.1.0

The first release: pull requests, issues and Actions for the repository you are in; diffs with review threads and
drafted line comments sent as one review; merging, checkout and worktrees; job logs; handing work to a Herdr agent.
