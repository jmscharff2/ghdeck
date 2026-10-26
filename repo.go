package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Repo is the GitHub repository ghdeck shows, and the checkout it was opened in, if any.
type Repo struct {
	Slug      string // owner/name
	Root      string // the checkout's top directory; empty when opened with -R outside one
	CommonDir string // git's common directory, shared by every worktree of the checkout
}

var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// remoteSlug reads owner/name from a GitHub remote URL in any of git's forms.
var remoteSlug = regexp.MustCompile(`^(?:https?://(?:[^@/]+@)?|ssh://(?:[^@/]+@)?|[^@/]+@)?([^/:]+)(?::\d+)?[:/]([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

func parseRemote(url string) (host, slug string, ok bool) {
	m := remoteSlug.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2] + "/" + m[3], true
}

// findRepo works out the repository for dir: the one `gh repo set-default` chose, else origin, else the first GitHub
// remote. explicit (-R owner/name) wins over all of them.
func findRepo(dir, explicit string) (Repo, error) {
	var r Repo
	if out, err := runIn(dir, "git", "rev-parse", "--show-toplevel"); err == nil {
		r.Root = strings.TrimSpace(string(out))
		if out, err := runIn(dir, "git", "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
			r.CommonDir = strings.TrimSpace(string(out))
		}
	}
	if explicit != "" {
		if !slugPattern.MatchString(explicit) {
			return r, fmt.Errorf("-R wants owner/name, not %q", explicit)
		}
		// The checkout here only counts if it is of that repository: its branch is no help otherwise.
		if here, err := remoteRepo(dir, r); err != nil || !strings.EqualFold(here.Slug, explicit) {
			r = Repo{}
		}
		r.Slug = explicit
		return r, nil
	}
	if r.Root == "" {
		return r, errors.New("not in a git checkout: run ghdeck inside one, or pass -R owner/name")
	}
	return remoteRepo(dir, r)
}

func remoteRepo(dir string, r Repo) (Repo, error) {
	want := or(os.Getenv("GH_HOST"), "github.com")
	out, _ := runIn(dir, "git", "config", "--get-regexp", `^remote\..*\.(url|gh-resolved)$`)
	urls, order := map[string]string{}, []string{}
	resolved := ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name := strings.TrimPrefix(key, "remote.")
		switch {
		case strings.HasSuffix(name, ".url"):
			name = strings.TrimSuffix(name, ".url")
			urls[name] = val
			order = append(order, name)
		case strings.HasSuffix(name, ".gh-resolved"):
			name = strings.TrimSuffix(name, ".gh-resolved")
			if val == "base" {
				resolved = name
			} else if slugPattern.MatchString(val) {
				r.Slug = val // set-default to a repo by name
				return r, nil
			}
		}
	}
	candidates := append([]string{resolved, "origin"}, order...)
	for _, name := range candidates {
		if host, slug, ok := parseRemote(urls[name]); ok && strings.EqualFold(host, want) {
			r.Slug = slug
			return r, nil
		}
	}
	return r, fmt.Errorf("no %s remote in %s: pass -R owner/name", want, r.Root)
}

// currentBranch is the checked-out branch, or "" when HEAD is detached or there is no checkout.
func currentBranch(root string) string {
	if root == "" {
		return ""
	}
	out, err := runIn(root, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// upstreamRepo says which repository a branch is pushed to, from its upstream: owner/name, or "" when it has none on
// GitHub. A branch pushed to your fork names the fork.
func upstreamRepo(root, branch string) string {
	if root == "" || branch == "" {
		return ""
	}
	remote, err := runIn(root, "git", "config", "--get", "branch."+branch+".remote")
	if err != nil {
		return ""
	}
	url, err := runIn(root, "git", "remote", "get-url", strings.TrimSpace(string(remote)))
	if err != nil {
		return ""
	}
	if _, slug, ok := parseRemote(string(url)); ok {
		return slug
	}
	return ""
}

// dirty says the checkout has uncommitted changes, which `gh pr checkout` would carry onto another branch.
func dirty(root string) bool {
	out, err := runIn(root, "git", "status", "--porcelain", "--untracked-files=no")
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}
