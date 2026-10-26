package main

import (
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// target is something to open straight away, from -open or a link clicked in Herdr: a pull request or issue, a pull
// request's diff, or an Actions run.
type target struct {
	repo   string // owner/name, or "" for this checkout's
	number int    // a pull request or issue
	files  bool   // the pull request's diff, from a /files link
	run    int64  // an Actions run
}

var (
	refPattern   = regexp.MustCompile(`^(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#?(\d+)$`)
	githubPath   = regexp.MustCompile(`^/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/(pull|issues)/(\d+)(/files|/changes)?`)
	githubRunURL = regexp.MustCompile(`^/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/actions/runs/(\d+)`)
)

// parseTarget reads 123, #123, owner/name#123, or a link to a pull request, issue or run on github.com (or $GH_HOST).
func parseTarget(s string) (target, bool) {
	s = strings.TrimSpace(s)
	if m := refPattern.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[2])
		return target{repo: m[1], number: n}, err == nil && n > 0
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), or(os.Getenv("GH_HOST"), "github.com")) {
		return target{}, false
	}
	if m := githubRunURL.FindStringSubmatch(u.Path); m != nil {
		id, err := strconv.ParseInt(m[2], 10, 64)
		return target{repo: m[1], run: id}, err == nil
	}
	if m := githubPath.FindStringSubmatch(u.Path); m != nil {
		n, err := strconv.Atoi(m[3])
		return target{repo: m[1], number: n, files: m[2] == "pull" && m[4] != ""}, err == nil && n > 0
	}
	return target{}, false
}

// view is the screen the target opens on.
func (t target) view(r Repo) subview {
	switch {
	case t.run > 0:
		return newRunView(r, t.run)
	case t.files:
		return newDiffView(r, t.number, "")
	}
	return newItemView(r, Item{Number: t.number})
}
