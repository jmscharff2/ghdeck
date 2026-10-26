package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestWrites changes a real repository: it opens a throwaway pull request and runs every write ghdeck makes against
// it, then closes the pull request and deletes its branch. It runs only when GHDECK_WRITE names a repository you own,
// say GHDECK_WRITE=you/scratch go test -run TestWrites -v .
func TestWrites(t *testing.T) {
	repo := os.Getenv("GHDECK_WRITE")
	if repo == "" {
		t.Skip("set GHDECK_WRITE=owner/name to run writes against a repository")
	}
	sharedRefresh()
	var base struct {
		Default string `json:"default_branch"`
	}
	must(t, ghGet("repos/"+repo, &base))
	var tip struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	must(t, ghGet(fmt.Sprintf("repos/%s/git/ref/heads/%s", repo, base.Default), &tip))
	var commit struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	must(t, ghGet(fmt.Sprintf("repos/%s/git/commits/%s", repo, tip.Object.SHA), &commit))
	if len(commit.Parents) == 0 {
		t.Skip("the default branch needs two commits, so the test branch can start behind it")
	}

	// A branch from the commit before the default branch's tip, so the pull request starts behind it.
	branch := fmt.Sprintf("ghdeck-write-test-%d", time.Now().Unix())
	must(t, ghWrite("POST", "repos/"+repo+"/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": commit.Parents[0].SHA}, nil))
	t.Cleanup(func() {
		if err := deleteBranch(repo, branch); err != nil {
			t.Errorf("cleanup: deleting %s: %v", branch, err)
		}
	})
	path := "ghdeck-write-test.txt"
	must(t, ghWrite("PUT", fmt.Sprintf("repos/%s/contents/%s", repo, path), map[string]string{
		"message": "ghdeck write test", "branch": branch,
		"content": base64.StdEncoding.EncodeToString([]byte("one\ntwo\nthree\n")),
	}, nil))

	it, err := createPR(repo, base.Default, branch, "ghdeck write test", "Opened and closed by ghdeck's TestWrites.", false)
	must(t, err)
	t.Logf("opened #%d", it.Number)
	t.Cleanup(func() {
		if err := setState(repo, it.Number, true, "closed"); err != nil {
			t.Errorf("cleanup: closing #%d: %v", it.Number, err)
		}
	})
	detail := func() *Detail {
		sharedRefresh()
		gh.forget("repos/" + repo)
		d, err := fetchDetail(repo, it.Number)
		must(t, err)
		return d
	}
	d := detail()

	t.Run("edit title and description", func(t *testing.T) {
		must(t, editItem(d.ID, true, "ghdeck write test (edited)", "Edited by TestWrites."))
		if got := detail(); got.Title != "ghdeck write test (edited)" || got.Body != "Edited by TestWrites." {
			t.Errorf("after editing: %q, %q", got.Title, got.Body)
		}
	})

	t.Run("comment, edit it, delete it", func(t *testing.T) {
		must(t, postComment(repo, it.Number, "a comment"))
		var mine *Event
		for _, e := range detail().Events {
			if e.Kind == "comment" && e.Body == "a comment" {
				e := e
				mine = &e
			}
		}
		if mine == nil || !mine.CanEdit || !mine.CanDelete {
			t.Fatalf("the comment, editable: %+v", mine)
		}
		must(t, editComment(mine.ID, "an edited comment"))
		must(t, deleteComment(mine.ID))
		for _, e := range detail().Events {
			if e.ID == mine.ID {
				t.Errorf("the comment is still there: %+v", e)
			}
		}
	})

	t.Run("labels", func(t *testing.T) {
		ls, err := fetchLabels(repo)
		must(t, err)
		if len(ls) == 0 {
			t.Skip("the repository has no labels")
		}
		must(t, setLabels(repo, it.Number, []string{ls[0].Name}))
		if got := detail().Labels; len(got) != 1 || got[0] != ls[0].Name {
			t.Errorf("labels = %v, want %s", got, ls[0].Name)
		}
		must(t, setLabels(repo, it.Number, nil))
	})

	t.Run("line comments, suggestions, resolving, editing, replying", func(t *testing.T) {
		d := detail()
		drafts := []DraftComment{
			{Path: path, Line: 2, Side: "RIGHT", Body: "```suggestion\nTWO\n```"},
			{Path: path, Line: 3, Side: "RIGHT", StartLine: 1, StartSide: "RIGHT", Body: "on three lines"},
		}
		must(t, submitReview(repo, it.Number, d.HeadSHA, "COMMENT", "", drafts))
		gh.forget("repos/" + repo)
		cs, err := fetchReviewComments(repo, it.Number)
		must(t, err)
		var single, multi *ReviewComment
		for i := range cs {
			switch {
			case cs[i].Line == 2:
				single = &cs[i]
			case cs[i].StartLine == 1 && cs[i].Line == 3:
				multi = &cs[i]
			}
		}
		if single == nil || multi == nil {
			t.Fatalf("both comments, the second on lines 1-3: %+v", cs)
		}
		if got, ok := suggestionIn(single.Body); !ok || len(got) != 1 || got[0] != "TWO" {
			t.Errorf("the suggestion: %q", got)
		}
		st, err := fetchReviewState(repo, it.Number)
		must(t, err)
		ts, ok := st.Threads[single.ID]
		if !ok {
			t.Fatalf("the review state should know the thread by its comment's id %d", single.ID)
		}
		must(t, setResolved(ts.ID, true))
		if st, _ := fetchReviewState(repo, it.Number); !st.Threads[single.ID].Resolved {
			t.Error("the thread should be resolved")
		}
		must(t, setResolved(ts.ID, false))
		must(t, replyToComment(repo, it.Number, single.ID, "a reply"))
		must(t, editReviewComment(repo, multi.ID, "edited, on three lines"))
		must(t, deleteReviewComment(repo, multi.ID))
	})

	t.Run("viewed", func(t *testing.T) {
		must(t, setViewed(d.ID, path, true))
		st, err := fetchReviewState(repo, it.Number)
		must(t, err)
		if !st.Viewed[path] {
			t.Error("the file should be viewed")
		}
		must(t, setViewed(d.ID, path, false))
	})

	t.Run("draft and ready", func(t *testing.T) {
		must(t, setDraft(d.ID, true))
		if !detail().Draft {
			t.Error("should be a draft")
		}
		must(t, setDraft(d.ID, false))
	})

	t.Run("reviewers", func(t *testing.T) {
		rs, err := fetchReviewers(repo, it.Number)
		must(t, err)
		if len(rs) == 0 {
			t.Skip("nobody else can review in this repository")
		}
		must(t, setReviewers(repo, it.Number, rs[:1], nil))
		must(t, setReviewers(repo, it.Number, nil, rs[:1]))
	})

	t.Run("auto-merge", func(t *testing.T) {
		d := detail()
		if !d.AutoAllowed {
			t.Skip("the repository does not allow auto-merge")
		}
		must(t, setAutoMerge(d.ID, d.Methods[0], d.HeadSHA, true))
		if detail().AutoMerge == "" {
			t.Error("auto-merge should be set")
		}
		must(t, setAutoMerge(d.ID, "", "", false))
	})

	t.Run("update branch", func(t *testing.T) {
		before := detail().HeadSHA
		must(t, updateBranch(d.ID, before, false))
		for i := 0; i < 10; i++ {
			time.Sleep(2 * time.Second)
			if after := detail().HeadSHA; after != before {
				t.Logf("updated: %s → %s", short(before), short(after))
				return
			}
		}
		t.Error("the branch should have a new head after updating it")
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(strings.TrimSpace(errText(err)))
	}
}
