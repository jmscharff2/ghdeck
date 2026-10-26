# ghdeck

A terminal deck for the GitHub repository you are working in. It shows the repo's pull requests, issues and Actions
runs, and opens with your branch's pull request one key away. You can review a diff line by line, draft comments
and send them as one review, approve, merge, read a failed job's log, and check a pull request out into a worktree,
all without leaving the terminal. Inside [Herdr](https://herdr.dev), any of it can be handed to a coding agent
running in another pane.

ghdeck uses the `gh` CLI's login. It stores no credentials and calls nothing but the GitHub API.

```
ghdeck  1 Pull requests 12  2 Issues 31  3 Actions
 me/my-app · on fix-login → #482 ✓ approved · b · 2 awaiting your review
 open · 12s ago
▸ #482  ● Fix the login redirect loop             ✓✔ +48−12      💬3   me             2h
  #479  ● Add rate limiting to the public API     ✗• +310−41     💬7   sam            5h
  #477  ○ draft · Move settings to a new page     ◐  +902−655          lee            1d
  #470  ● Bump the Go toolchain                   ✓• +3−3              renovate       3d
 tab list · ↑↓ enter open · d diff · f filter · / search · a agent · ? more · q quit
```

## Install

Needs Go 1.24+ and the [GitHub CLI](https://cli.github.com), signed in (`gh auth login`).

```bash
go install github.com/jmscharff2/ghdeck@latest
```

Without Go, download a binary for macOS or Linux from the [releases](https://github.com/jmscharff2/ghdeck/releases)
and put it on your `PATH`.

Run it anywhere inside a checkout. It finds the repository from the remote that `gh repo set-default` chose, else
`origin`, else the first GitHub remote. `ghdeck -R owner/name` opens any repository, and `ghdeck -branch` opens
straight onto the pull request for the checked-out branch. `ghdeck -open` takes a number, `owner/name#123`, or a link
to a pull request, issue or Actions run; a pull request's `/files` link opens its diff.

### As a Herdr plugin

```bash
herdr plugin install jmscharff2/ghdeck      # builds it with Go; needs Herdr 0.9.3 or later
```

This adds three actions to Herdr, each for the repository in the pane you are working in:

- **ghdeck: open beside this pane**
- **ghdeck: open in a new tab**
- **ghdeck: review this branch's pull request**, which opens on your branch's PR

To work on ghdeck itself, `herdr plugin link <path to your checkout>` uses your local build instead.

To open it with a key, bind the action, or add a popup to Herdr's `config.toml` (with ghdeck on your `PATH`, from
`go install`):

```toml
[[keys.command]]
key = ["cmd+shift+h", "prefix+h"]
type = "popup"
command = "cd \"${HERDR_ACTIVE_PANE_CWD:-.}\" && exec ghdeck"
description = "GitHub deck"
width = "95%"
height = "90%"

[[keys.command]]
key = ["prefix+P"]
type = "plugin_action"
command = "ghdeck.open"
```

Inside Herdr, ghdeck also:

- **opens interactive commands in a pane below** (`gh issue create`, `gh pr create` for a branch not pushed yet,
  checking a branch out, signing in). Outside Herdr they borrow the terminal until they exit.
- **opens a pull request's worktree as a Herdr workspace** (`W`), checked out beside your checkout, so you can run it
  or give it to an agent without touching your branch.
- **works on a pull request with an agent, in one key** (`w`): it checks the branch out in a worktree beside yours,
  opens that as a Herdr workspace, and starts an agent there (Claude Code, Codex, or whichever is installed), or uses
  the one already in it. The prompt names the unresolved threads and failing checks, and you see it, and can edit it,
  first. Then ghdeck watches the pull request and tells you when the agent pushes and how the checks come out.
- **puts each workspace's pull request in the sidebar**, as the `$gh_pr` token: `#482 ✓ approved` under every workspace
  whose checkout or worktree is on that pull request's branch. It costs nothing extra from GitHub, and goes when ghdeck
  does. Name it in a Space row in Herdr's `config.toml`, coloured by what it says:

  ```toml
  [ui.sidebar.spaces]
  rows = [
    ["state_icon", "workspace"],
    ["branch", { token = "$gh_pr", rules = [
      { contains = "✗", fg = "#ff6369" },
      { contains = "◐", fg = "#f5a524" },
      { contains = "✓", fg = "#4cc38a" },
    ] }],
  ]
  ```
- **shows which agent is on which pull request.** A pull request whose branch an agent in this repo has checked out
  is marked ◆ with the agent, coloured by whether it is working, waiting on you or done. `J` switches to its pane.
- **hands work to an agent** (`a`): a pull request, an issue, failing checks or a failed run. It lists the agents
  Herdr is running, the ones working in this repo first, and shows the prompt it will send, which `e` lets you edit
  first. If no agent fits, it can start Claude Code in a new pane.
- **sends notifications** when your branch's checks fail or pass, when it is approved or changes are requested, and
  when someone asks for your review.

## Screens

**Lists.** Pull requests, issues and Actions runs, a tab each. `f` steps through preset filters. For pull requests
they are open, review requested, mine and closed; for issues, open, assigned to me, mine and closed. `/` takes a
GitHub search (`label:bug author:sam`, `is:merged`, `sort:created-asc`) in place of the preset. The header shows your
branch's pull request with its checks and review state, and how many pull requests are waiting on your review.

**A pull request.** Its merge state, a summary of its checks and reviewers, the description, the checks (failing
ones first), unresolved review threads, and the conversation: comments, reviews, commits, merges.

**The diff.** One file at a time, its code highlighted for the language and added and removed lines tinted, with each review thread under the line it is on and outdated threads at the top
of their file. `c` drafts a comment on the line under the cursor. Drafts are saved as you write them (in
`~/Library/Caches/ghdeck` on macOS, `~/.cache/ghdeck` on Linux), so closing the pane loses nothing. `v` sends every
draft as one review, with a verdict and a summary.

**A new pull request** (`n`). Choose the branch to merge, your checked-out branch first or any branch on GitHub,
then the branch it goes into. ghdeck has no settings for how your team works; it reads that from where the
repository's pull requests went, and puts the likeliest target first, saying why:

- a branch that went somewhere before goes there again: `staging` into `main`, `develop` into `staging`
- a branch in a family follows it: `release/1.5` goes where `release/1.4` went
- a feature branch goes where feature branches go: `develop`, in a repository that works through `develop`
- with no history to go on, the default branch.

Typing filters either list, and names a branch that is not listed. ghdeck then shows what the merge would bring: the
commits, the files, whether the branch is behind, any pull request already open between the two, and a warning when
a long-lived branch would go into a feature branch. A promotion such as `staging` into `main` starts as "Merge staging
into main" with the pull requests it carries listed, as release notes, and any commits pushed straight to the branch
after them. A single commit starts with its own message. The repository's pull request template comes first. The
first line is the title. It can open as a draft. Once it is created, ghdeck asks who should review it, GitHub's
suggestions first. A branch that is not pushed yet, or is on your fork, goes to `gh pr create`, which pushes it and
handles forks.

**A run.** Its jobs, with the chosen job's steps beneath it, live while it runs. `enter` streams the job's log with
a header for each step.

## Keys

**Lists**

| Key | Does |
|---|---|
| `tab` `shift+tab` `1`-`3` | pull requests, issues, Actions |
| `↑` `↓` `enter` | choose and open |
| `f` | next filter (Actions: all branches, or this one) |
| `/` | search with GitHub's syntax; `esc` clears it |
| `d` | the chosen pull request's diff |
| `b` | your branch's pull request |
| `n` | new pull request from any branch into any other (Issues: `gh issue create`) |
| `C` | check the pull request out here (asks first, and warns about uncommitted changes) |
| `W` | check it out in a worktree beside this checkout and, in Herdr, open that as a workspace |
| `w` | work on it: the worktree, an agent in it with what is left to do, and news of its pushes and checks (Herdr) |
| `a` | hand the pull request or issue to an agent (Herdr) |
| `J` | switch to the agent working on the pull request's branch (Herdr) |
| `o` | open on GitHub |
| `A` | `gh auth login`, when the login has expired |
| `r` `?` `q` | refresh, more keys, quit |

**A pull request or issue**

| Key | Does |
|---|---|
| `↑` `↓` `pgup` `pgdn` `g` `G` | scroll |
| `c` | comment |
| `d` `s` | the diff, every check (enter on an Actions check opens its run) |
| `v` | review: comment, approve or request changes, with the diff's drafted line comments |
| `m` | merge, by any method the repo allows, optionally deleting the branch, or set it to merge itself once checks pass and reviews allow (type `merge`) |
| `D` | convert to draft, or mark ready for review |
| `x` | close or reopen (type `yes`) |
| `A` | assign yourself |
| `R` | reviewers: GitHub's suggestions first, teams, and anyone who can be assigned (space ticks, typing filters) |
| `L` | labels |
| `C` `W` | check out here, or in a worktree |
| `w` | work on it with an agent in its own worktree (Herdr) |
| `p` | an issue's linked pull request |
| `a` `o` `r` | hand to an agent, open on GitHub, refresh |

**The diff**

| Key | Does |
|---|---|
| `↑` `↓` `pgup` `pgdn` `g` `G` | move the cursor line |
| `n` `p` (or `]` `[`) | next, previous file |
| `f` | every file, with its open threads and whether you viewed it; typing filters |
| `V` | mark the file viewed, as GitHub's checkbox does, and go on to the next one you haven't |
| `S` | only what changed since your last review, or everything again |
| `{` `}` | previous, next hunk |
| `t` `T` | next, previous line with an open thread or a draft, across files |
| `c` | draft a comment on this line |
| `C` | suggest a change to this line: a draft that starts as the line, to edit into what it should be |
| `a` | apply the suggested change on this line to your checkout, when it is at the pull request's head (asks first; you commit) |
| `e` `x` | edit, delete your draft on this line |
| `R` | reply to the thread on this line (posts at once) |
| `z` | resolve the thread on this line, or open it again; resolved threads fold to a line |
| `v` | send the review |
| `s` | old and new side by side, in a pane 120 columns or wider |
| `w` | wrap long lines on or off |
| `o` | the files on GitHub |

**Writing.** `ctrl+s` sends, `ctrl+e` finishes the text in `$EDITOR`, and `esc` twice throws it away.

## How it uses GitHub

Lists and pull request screens each come from one GraphQL query. Diffs, review comments and Actions come from the
REST API, asked for again with the last response's ETag. GitHub answers 304 when nothing changed, and a 304 does not
count against the hourly limit. GraphQL has no such shortcut, so every refresh there is paid for: about 2 points for
a list, 1 for the header or a pull request's screen, out of 5,000 an hour that `gh` and your agents share.

Lists refresh every 30 seconds (every 10 while an Actions run on screen is moving), the header once a minute, and a
pull request's screen every minute, or every 15 seconds while its checks run. That is about 300 points an hour while
you use it. After 10 minutes with no key pressed, everything slows to once every 2 minutes, and after an hour to once
every 10, so a deck left open in a pane costs about 20 points an hour. Decks open at the same time share what they
fetch: an answer one got in the last 20 seconds serves the others, so two panes on a repository cost about what one
does. (The answers are kept in your cache directory, readable only by you, and any change you make clears them.) The next key refreshes at once. When either
hourly budget falls below 1,000, everything slows to once every 2 minutes. When a limit is reached, ghdeck stops asking
until the reset time it shows. `GH_HOST` points it at GitHub Enterprise.

## Security

- **Text from GitHub is cleaned before it reaches your terminal.** Anyone who can open an issue writes text ghdeck
  shows: titles, descriptions, comments, diffs, branch names, logs. Escape sequences and control characters are
  removed, so a comment cannot set your clipboard or redraw your screen.
- **Agents get references, not text.** A prompt names the pull request or issue and points the agent at `gh` to read
  it. It never pastes in the description or comments, which a stranger may have written to steer an agent. You see
  the prompt before it is sent.
- **Changes ask for a typed word**: `merge` to merge, `yes` to close, reopen, check out or re-run. A single key never
  confirms, and pasted text does not count. A merge is sent with the head commit you reviewed, so GitHub refuses it
  if someone pushed since.
- **Only `https` links open in the browser.**
- **ghdeck stores no credentials.** It uses `gh auth token`, and sends the token only to the API host.

What it does not protect against: anything that already runs as you. A program that can type into the ghdeck pane
(Herdr lets agents send keys) can type a confirmation word too; the typed word stops slips, not a determined program.

## Development

```bash
go test ./...
GHDECK_LIVE=1 go test -run TestLive .    # reads a public repo through the real API
go build -o ghdeck . && ./ghdeck -R owner/name
```

CI runs the same checks on every pull request. Pushing a tag such as `v0.2.0` publishes a release with binaries.
