package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Saved searches add to the preset filters f steps through: "my team's", "needs QA", whatever a GitHub search can
// say. Yours live in ~/.config/ghdeck/config.json (or $XDG_CONFIG_HOME), for every repository or for one; a team's
// can live in the repository, as .ghdeck.json at its top. Either holds searches only, never commands: the worst a
// repository's file can do is search more widely. Its text is still cleaned, and it must be a small plain file.
//
//	{
//	  "pull_requests": [{"name": "my team", "query": "is:open team-review-requested:acme/web"}],
//	  "issues": [{"name": "needs QA", "query": "is:open label:qa"}],
//	  "repos": {"acme/web": {"pull_requests": [{"name": "release", "query": "is:open base:release"}]}}
//	}

type savedSearch struct {
	Name  string `json:"name"`
	Query string `json:"query"`
}

type searches struct {
	PullRequests []savedSearch `json:"pull_requests"`
	Issues       []savedSearch `json:"issues"`
}

type config struct {
	searches
	Repos map[string]searches `json:"repos"`
}

func configPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "ghdeck", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "ghdeck", "config.json")
}

// loadFilters is the preset filters for pull requests and issues, then the saved searches for this repository:
// yours for every repository, yours for this one, and the repository's own. A file that does not parse is reported,
// and the presets still work.
func loadFilters(r Repo) (prs, issues []filter, err error) {
	prs, issues = append([]filter{}, prFilters...), append([]filter{}, issueFilters...)
	add := func(s searches) {
		// A repository's file is anyone's: its text is cleaned like everything else from outside.
		for _, f := range s.PullRequests {
			if name, q := clean(f.Name), clean(f.Query); name != "" && strings.TrimSpace(q) != "" {
				prs = append(prs, filter{name: name, query: q})
			}
		}
		for _, f := range s.Issues {
			if name, q := clean(f.Name), clean(f.Query); name != "" && strings.TrimSpace(q) != "" {
				issues = append(issues, filter{name: name, query: q})
			}
		}
	}
	read := func(path string) (config, error) {
		var c config
		if path == "" {
			return c, nil
		}
		// A plain file, and a small one: a repository could commit a symlink to /dev/zero, or to a pipe.
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return c, nil
		}
		if err != nil {
			return c, err
		}
		if !info.Mode().IsRegular() || info.Size() > 256<<10 {
			return c, fmt.Errorf("%s: not a plain file under 256 KB; ignored", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
		return c, nil
	}
	var errs []string
	if c, e := read(configPath()); e != nil {
		errs = append(errs, e.Error())
	} else {
		add(c.searches)
		for slug, s := range c.Repos {
			if strings.EqualFold(slug, r.Slug) {
				add(s)
			}
		}
	}
	if r.Root != "" {
		if c, e := read(filepath.Join(r.Root, ".ghdeck.json")); e != nil {
			errs = append(errs, e.Error())
		} else {
			add(c.searches)
		}
	}
	if len(errs) > 0 {
		err = fmt.Errorf("saved searches: %s", strings.Join(errs, "; "))
	}
	return prs, issues, err
}
