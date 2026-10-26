package main

import (
	"os"
	"testing"
)

// TestLive reads a real public repository. It runs only with GHDECK_LIVE=1, and only reads.
func TestLive(t *testing.T) {
	if os.Getenv("GHDECK_LIVE") != "1" {
		t.Skip("set GHDECK_LIVE=1 to read GitHub")
	}
	repo := "dlvhdr/gh-dash"
	l, err := fetchList(searchQuery(repo, true, prFilters[0], ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("open PRs: %d, first: %+v", l.Total, l.Items[0])
	d, err := fetchDetail(repo, l.Items[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("detail: checks=%d threads=%d events=%d methods=%v merge=%s/%s rollup=%s", len(d.Checks), len(d.Threads), len(d.Events), d.Methods, d.Mergeable, d.MergeWhy, d.Item.Checks)
	files, err := fetchFiles(repo, d.Number)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("files: %d, first %s lines=%d", len(files), files[0].Path, len(parsePatch(files[0].Patch)))
	cs, err := fetchReviewComments(repo, d.Number)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("review comments: %d", len(cs))
	il, err := fetchList(searchQuery(repo, false, issueFilters[0], ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("open issues: %d", il.Total)
	id, err := fetchDetail(repo, il.Items[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("issue: %q events=%d linked=%d", id.Title, len(id.Events), len(id.Linked))
	s, err := fetchStatus(repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status: viewer=%s branchPR=%v reviewFor=%d", s.Viewer, s.BranchPR != nil, len(s.ReviewFor))
	runs, err := fetchRuns(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("runs: %d, first %s %s %s", len(runs), runs[0].Name, runs[0].Status, runs[0].Conclusion)
	s2, err := fetchStatus("jmscharff2/GCDeck", "")
	t.Logf("gcdeck status: %+v %v", s2, err)
}
