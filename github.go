package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Item is a pull request or an issue as a list shows it.
type Item struct {
	Number    int
	Title     string
	State     string // OPEN, CLOSED, MERGED
	Author    string
	URL       string
	IsPR      bool
	Draft     bool
	Updated   time.Time
	Comments  int
	Labels    []string
	Assignees []string

	// Pull requests only.
	Head, Base string
	HeadSHA    string
	Review     string // reviewDecision: APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, or ""
	Checks     string // the head commit's rollup: SUCCESS, FAILURE, ERROR, PENDING, EXPECTED, or ""
	Additions  int
	Deletions  int
}

// GraphQL shapes shared by the queries below.
type gqlLogin struct {
	Login string `json:"login"`
}

type gqlNames struct {
	Nodes []struct {
		Name  string `json:"name"`
		Login string `json:"login"`
	} `json:"nodes"`
}

func (n gqlNames) list() []string {
	var out []string
	for _, x := range n.Nodes {
		out = append(out, clean(or(x.Name, x.Login)))
	}
	return out
}

type gqlItem struct {
	Typename   string    `json:"__typename"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	State      string    `json:"state"`
	URL        string    `json:"url"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Author     *gqlLogin `json:"author"`
	IsDraft    bool      `json:"isDraft"`
	HeadRef    string    `json:"headRefName"`
	BaseRef    string    `json:"baseRefName"`
	HeadOID    string    `json:"headRefOid"`
	Review     string    `json:"reviewDecision"`
	Additions  int       `json:"additions"`
	Deletions  int       `json:"deletions"`
	Labels     gqlNames  `json:"labels"`
	Assignees  gqlNames  `json:"assignees"`
	CommentsCt struct {
		Total int `json:"totalCount"`
	} `json:"comments"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				Rollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

const prFields = `
		number title state url updatedAt isDraft headRefName baseRefName headRefOid reviewDecision additions deletions
		author { login }
		comments { totalCount }
		labels(first: 6) { nodes { name } }
		assignees(first: 4) { nodes { login } }
		commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }`

const itemFields = `
	__typename
	... on PullRequest {` + prFields + `
	}
	... on Issue {
		number title state url updatedAt
		author { login }
		comments { totalCount }
		labels(first: 6) { nodes { name } }
		assignees(first: 4) { nodes { login } }
	}`

func (g gqlItem) item() Item {
	it := Item{
		Number: g.Number, Title: clean(g.Title), State: clean(g.State), URL: clean(g.URL), Updated: g.UpdatedAt,
		IsPR: g.Typename == "PullRequest", Draft: g.IsDraft, Comments: g.CommentsCt.Total,
		Labels: g.Labels.list(), Assignees: g.Assignees.list(),
		Head: clean(g.HeadRef), Base: clean(g.BaseRef), Review: clean(g.Review), Additions: g.Additions, Deletions: g.Deletions,
		HeadSHA: clean(g.HeadOID),
	}
	if g.Author != nil {
		it.Author = clean(g.Author.Login)
	} else {
		it.Author = "ghost"
	}
	if n := g.Commits.Nodes; len(n) > 0 && n[0].Commit.Rollup != nil {
		it.Checks = clean(n[0].Commit.Rollup.State)
	}
	return it
}

// filter is one of a list's preset searches.
type filter struct {
	name  string
	query string // added to "repo:owner/name is:pr" or "is:issue"
}

var prFilters = []filter{
	{"open", "is:open"},
	{"review requested", "is:open review-requested:@me"},
	{"mine", "is:open author:@me"},
	{"closed", "is:closed"},
}

var issueFilters = []filter{
	{"open", "is:open"},
	{"assigned to me", "is:open assignee:@me"},
	{"mine", "is:open author:@me"},
	{"closed", "is:closed"},
}

// searchQuery builds a GitHub search for a list. extra is what you typed after /, in GitHub's search syntax.
func searchQuery(repo string, prs bool, f filter, extra string) string {
	kind := "is:issue"
	if prs {
		kind = "is:pr"
	}
	q := "repo:" + repo + " " + kind + " "
	if extra != "" {
		q += extra
	} else {
		q += f.query
	}
	if !strings.Contains(q, "sort:") {
		q += " sort:updated-desc"
	}
	return q
}

// List is one page of search results.
type List struct {
	Items []Item
	Total int
}

func fetchList(query string) (List, error) {
	var resp struct {
		Search struct {
			Count int       `json:"issueCount"`
			Nodes []gqlItem `json:"nodes"`
		} `json:"search"`
	}
	err := ghQuery(`query($q: String!) { search(query: $q, type: ISSUE, first: 50) { issueCount nodes {`+itemFields+`} } }`,
		map[string]any{"q": query}, &resp)
	if err != nil {
		return List{}, err
	}
	l := List{Total: resp.Search.Count}
	for _, n := range resp.Search.Nodes {
		if n.Number != 0 {
			l.Items = append(l.Items, n.item())
		}
	}
	return l, nil
}

// Status is what the header shows whatever tab is open: your branch's pull request and what waits on your review.
type Status struct {
	Viewer    string
	BranchPR  *Item
	ReviewFor []Item       // open pull requests asking for your review
	Watched   map[int]Item // pull requests handed to an agent, watched for pushes and checks
}

func fetchStatus(repo, branch string, watched ...int) (Status, error) {
	owner, name, _ := strings.Cut(repo, "/")
	q := `query($o: String!, $n: String!, $b: String!, $rq: String!, $hasBranch: Boolean!) {
		viewer { login }
		repository(owner: $o, name: $n) {
			pullRequests(headRefName: $b, first: 1, states: [OPEN], orderBy: {field: UPDATED_AT, direction: DESC}) @include(if: $hasBranch) {
				nodes { __typename ` + prFields + ` }
			}
		}
		review: search(query: $rq, type: ISSUE, first: 20) { nodes {` + itemFields + `} }
	}`
	var resp struct {
		Viewer     gqlLogin `json:"viewer"`
		Repository *struct {
			PRs *struct {
				Nodes []gqlItem `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
		Review struct {
			Nodes []gqlItem `json:"nodes"`
		} `json:"review"`
	}
	err := ghQuery(q, map[string]any{
		"o": owner, "n": name, "b": branch, "hasBranch": branch != "",
		"rq": "repo:" + repo + " is:pr is:open review-requested:@me",
	}, &resp)
	if err != nil {
		return Status{}, err
	}
	s := Status{Viewer: clean(resp.Viewer.Login), Watched: fetchWatched(owner, name, watched)}
	if r := resp.Repository; r != nil && r.PRs != nil && len(r.PRs.Nodes) > 0 {
		it := r.PRs.Nodes[0].item()
		s.BranchPR = &it
	}
	for _, n := range resp.Review.Nodes {
		if n.Number != 0 {
			s.ReviewFor = append(s.ReviewFor, n.item())
		}
	}
	return s, nil
}

// fetchWatched reads the pull requests handed to agents, in a query of their own: one that can no longer be found
// (deleted, or its repository moved) leaves the others, and the header, unharmed.
func fetchWatched(owner, name string, numbers []int) map[int]Item {
	out := map[int]Item{}
	if len(numbers) == 0 {
		return out
	}
	var w strings.Builder
	for _, n := range numbers {
		fmt.Fprintf(&w, "w%d: pullRequest(number: %d) { __typename %s }\n", n, n, prFields)
	}
	var raw struct {
		Repository map[string]json.RawMessage `json:"repository"`
	}
	q := `query($o: String!, $n: String!) { repository(owner: $o, name: $n) { ` + w.String() + ` } }`
	if ghQuery(q, map[string]any{"o": owner, "n": name}, &raw) != nil {
		return out
	}
	for _, n := range numbers {
		var g gqlItem
		if b := raw.Repository[fmt.Sprintf("w%d", n)]; len(b) > 0 && json.Unmarshal(b, &g) == nil && g.Number != 0 {
			out[n] = g.item()
		}
	}
	return out
}

// ---- one pull request or issue in full ----

// Event is one entry in a conversation: a comment, a review, a commit, a state change.
type Event struct {
	Kind   string // comment, review, commit, merged, closed, reopened
	Author string
	Body   string
	State  string // a review's: APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	At     time.Time
	Extra  string // a commit's short SHA, a review's comment count
}

// Check is one status check on a pull request's head commit.
type Check struct {
	Name       string
	Status     string // QUEUED, IN_PROGRESS, COMPLETED, … or a status context's state
	Conclusion string // SUCCESS, FAILURE, …
	URL        string
	RunID      int64 // the Actions run behind a check run, when there is one
}

// Thread is a review thread on a pull request's diff.
type Thread struct {
	Path     string
	Line     int
	Resolved bool
	Outdated bool
	Comments []Event
}

// Detail is a pull request or issue with everything its screen shows.
type Detail struct {
	Item
	ID        string // GraphQL node id, for mutations
	Body      string
	Created   time.Time
	HeadSHA   string
	Changed   int
	Mergeable string // MERGEABLE, CONFLICTING, UNKNOWN
	MergeWhy  string // mergeStateStatus: BLOCKED, BEHIND, CLEAN, DIRTY, UNSTABLE, …
	Fork      bool   // the head branch lives in another repository
	Reviewers []string
	Reviews   []Event // the latest review from each reviewer
	Checks    []Check
	Threads   []Thread
	Events    []Event
	Linked    []Item // pull requests that will close this issue
	Methods   []string

	AutoAllowed bool   // the repository lets pull requests merge themselves once they can
	AutoMerge   string // the method set to merge with once checks and reviews allow, or ""
	AutoBy      string
}

const detailQuery = `query($o: String!, $n: String!, $num: Int!) {
	repository(owner: $o, name: $n) {
		squashMergeAllowed mergeCommitAllowed rebaseMergeAllowed viewerDefaultMergeMethod autoMergeAllowed
		issueOrPullRequest(number: $num) {
			__typename
			... on PullRequest {
				id number title state url body createdAt updatedAt isDraft headRefName baseRefName headRefOid
				reviewDecision additions deletions changedFiles mergeable mergeStateStatus isCrossRepository
				autoMergeRequest { mergeMethod enabledBy { login } }
				author { login }
				comments { totalCount }
				labels(first: 100) { nodes { name } }
				assignees(first: 10) { nodes { login } }
				reviewRequests(first: 10) { nodes { requestedReviewer { ... on User { login } ... on Team { name } } } }
				latestReviews(first: 20) { nodes { author { login } state submittedAt } }
				commits(last: 1) { nodes { commit { statusCheckRollup { state contexts(first: 100) { nodes {
					__typename
					... on CheckRun { name status conclusion detailsUrl checkSuite { workflowRun { databaseId } } }
					... on StatusContext { context state targetUrl }
				} } } } } }
				reviewThreads(first: 100) { nodes { isResolved isOutdated path line originalLine
					comments(first: 30) { nodes { author { login } body createdAt } } } }
				timelineItems(last: 60, itemTypes: [ISSUE_COMMENT, PULL_REQUEST_REVIEW, PULL_REQUEST_COMMIT, MERGED_EVENT, CLOSED_EVENT, REOPENED_EVENT]) {
					nodes {
						__typename
						... on IssueComment { author { login } body createdAt }
						... on PullRequestReview { author { login } body state submittedAt comments { totalCount } }
						... on PullRequestCommit { commit { oid messageHeadline committedDate author { user { login } name } } }
						... on MergedEvent { actor { login } createdAt }
						... on ClosedEvent { actor { login } createdAt }
						... on ReopenedEvent { actor { login } createdAt }
					}
				}
			}
			... on Issue {
				id number title state url body createdAt updatedAt
				author { login }
				comments { totalCount }
				labels(first: 100) { nodes { name } }
				assignees(first: 10) { nodes { login } }
				closedByPullRequestsReferences(first: 5) { nodes { number title state url } }
				timelineItems(last: 60, itemTypes: [ISSUE_COMMENT, CLOSED_EVENT, REOPENED_EVENT]) {
					nodes {
						__typename
						... on IssueComment { author { login } body createdAt }
						... on ClosedEvent { actor { login } createdAt }
						... on ReopenedEvent { actor { login } createdAt }
					}
				}
			}
		}
	}
}`

type gqlEvent struct {
	Typename    string    `json:"__typename"`
	Author      *gqlLogin `json:"author"`
	Actor       *gqlLogin `json:"actor"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"createdAt"`
	SubmittedAt time.Time `json:"submittedAt"`
	Comments    struct {
		Total int `json:"totalCount"`
	} `json:"comments"`
	Commit struct {
		OID      string    `json:"oid"`
		Headline string    `json:"messageHeadline"`
		Date     time.Time `json:"committedDate"`
		Author   struct {
			User *gqlLogin `json:"user"`
			Name string    `json:"name"`
		} `json:"author"`
	} `json:"commit"`
}

func login(l *gqlLogin) string {
	if l == nil {
		return "ghost"
	}
	return clean(l.Login)
}

func (g gqlEvent) event() (Event, bool) {
	switch g.Typename {
	case "IssueComment":
		return Event{Kind: "comment", Author: login(g.Author), Body: cleanBody(g.Body), At: g.CreatedAt}, true
	case "PullRequestReview":
		// A review with no summary and only line comments is shown by its threads; keep it for its verdict.
		e := Event{Kind: "review", Author: login(g.Author), Body: cleanBody(g.Body), State: clean(g.State), At: g.SubmittedAt}
		if g.Comments.Total > 0 {
			e.Extra = fmt.Sprintf("%d line comment(s)", g.Comments.Total)
		}
		return e, true
	case "PullRequestCommit":
		who := clean(g.Commit.Author.Name)
		if g.Commit.Author.User != nil {
			who = clean(g.Commit.Author.User.Login)
		}
		return Event{Kind: "commit", Author: who, Body: clean(g.Commit.Headline), At: g.Commit.Date, Extra: short(clean(g.Commit.OID))}, true
	case "MergedEvent":
		return Event{Kind: "merged", Author: login(g.Actor), At: g.CreatedAt}, true
	case "ClosedEvent":
		return Event{Kind: "closed", Author: login(g.Actor), At: g.CreatedAt}, true
	case "ReopenedEvent":
		return Event{Kind: "reopened", Author: login(g.Actor), At: g.CreatedAt}, true
	}
	return Event{}, false
}

// cleanBody cleans a description or comment, keeping its lines.
func cleanBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = clean(l)
	}
	return strings.Join(lines, "\n")
}

func fetchDetail(repo string, number int) (*Detail, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var resp struct {
		Repository *struct {
			Squash  bool   `json:"squashMergeAllowed"`
			Merge   bool   `json:"mergeCommitAllowed"`
			Rebase  bool   `json:"rebaseMergeAllowed"`
			Default string `json:"viewerDefaultMergeMethod"`
			Auto    bool   `json:"autoMergeAllowed"`
			Node    *struct {
				gqlItem
				ID         string    `json:"id"`
				Body       string    `json:"body"`
				CreatedAt  time.Time `json:"createdAt"`
				HeadOID    string    `json:"headRefOid"`
				Changed    int       `json:"changedFiles"`
				Mergeable  string    `json:"mergeable"`
				MergeState string    `json:"mergeStateStatus"`
				CrossRepo  bool      `json:"isCrossRepository"`
				AutoMerge  *struct {
					Method string   `json:"mergeMethod"`
					By     gqlLogin `json:"enabledBy"`
				} `json:"autoMergeRequest"`
				ReviewReqs struct {
					Nodes []struct {
						Reviewer *struct {
							Login string `json:"login"`
							Name  string `json:"name"`
						} `json:"requestedReviewer"`
					} `json:"nodes"`
				} `json:"reviewRequests"`
				Latest struct {
					Nodes []gqlEvent `json:"nodes"`
				} `json:"latestReviews"`
				Commits2 struct {
					Nodes []struct {
						Commit struct {
							Rollup *struct {
								State    string `json:"state"`
								Contexts struct {
									Nodes []struct {
										Typename   string `json:"__typename"`
										Name       string `json:"name"`
										Status     string `json:"status"`
										Conclusion string `json:"conclusion"`
										DetailsURL string `json:"detailsUrl"`
										Suite      *struct {
											Run *struct {
												ID int64 `json:"databaseId"`
											} `json:"workflowRun"`
										} `json:"checkSuite"`
										Context   string `json:"context"`
										State     string `json:"state"`
										TargetURL string `json:"targetUrl"`
									} `json:"nodes"`
								} `json:"contexts"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
				Threads struct {
					Nodes []struct {
						Resolved     bool   `json:"isResolved"`
						Outdated     bool   `json:"isOutdated"`
						Path         string `json:"path"`
						Line         int    `json:"line"`
						OriginalLine int    `json:"originalLine"`
						Comments     struct {
							Nodes []gqlEvent `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
				Timeline struct {
					Nodes []gqlEvent `json:"nodes"`
				} `json:"timelineItems"`
				Closers struct {
					Nodes []gqlItem `json:"nodes"`
				} `json:"closedByPullRequestsReferences"`
			} `json:"issueOrPullRequest"`
		} `json:"repository"`
	}
	if err := ghQuery(detailQuery, map[string]any{"o": owner, "n": name, "num": number}, &resp); err != nil {
		return nil, err
	}
	if resp.Repository == nil || resp.Repository.Node == nil {
		return nil, fmt.Errorf("#%d not found in %s", number, repo)
	}
	n := resp.Repository.Node
	d := &Detail{
		Item: n.item(), ID: n.ID, Body: cleanBody(n.Body), Created: n.CreatedAt, HeadSHA: clean(n.HeadOID),
		Changed: n.Changed, Mergeable: clean(n.Mergeable), MergeWhy: clean(n.MergeState), Fork: n.CrossRepo,
		AutoAllowed: resp.Repository.Auto,
	}
	for _, r := range n.ReviewReqs.Nodes {
		if r.Reviewer != nil {
			d.Reviewers = append(d.Reviewers, clean(or(r.Reviewer.Login, r.Reviewer.Name)))
		}
	}
	for _, r := range n.Latest.Nodes {
		d.Reviews = append(d.Reviews, Event{Kind: "review", Author: login(r.Author), State: clean(r.State), At: r.SubmittedAt})
	}
	// Commits2 shadows the list fields' commits, so the rollup state is read from it.
	for _, c := range n.Commits2.Nodes {
		if c.Commit.Rollup == nil {
			continue
		}
		d.Item.Checks = clean(c.Commit.Rollup.State)
		for _, x := range c.Commit.Rollup.Contexts.Nodes {
			ch := Check{Name: clean(x.Name), Status: clean(x.Status), Conclusion: clean(x.Conclusion), URL: clean(x.DetailsURL)}
			if x.Typename == "StatusContext" {
				ch = Check{Name: clean(x.Context), Status: "COMPLETED", Conclusion: clean(x.State), URL: clean(x.TargetURL)}
				if x.State == "PENDING" || x.State == "EXPECTED" {
					ch.Status, ch.Conclusion = "IN_PROGRESS", ""
				}
			}
			if x.Suite != nil && x.Suite.Run != nil {
				ch.RunID = x.Suite.Run.ID
			}
			d.Checks = append(d.Checks, ch)
		}
	}
	sort.SliceStable(d.Checks, func(i, j int) bool { return checkRank(d.Checks[i]) < checkRank(d.Checks[j]) })
	for _, t := range n.Threads.Nodes {
		th := Thread{Path: clean(t.Path), Line: or0(t.Line, t.OriginalLine), Resolved: t.Resolved, Outdated: t.Outdated}
		for _, c := range t.Comments.Nodes {
			th.Comments = append(th.Comments, Event{Kind: "comment", Author: login(c.Author), Body: cleanBody(c.Body), At: c.CreatedAt})
		}
		d.Threads = append(d.Threads, th)
	}
	for _, e := range n.Timeline.Nodes {
		if ev, ok := e.event(); ok {
			d.Events = append(d.Events, ev)
		}
	}
	for _, c := range n.Closers.Nodes {
		d.Linked = append(d.Linked, Item{Number: c.Number, Title: clean(c.Title), State: clean(c.State), URL: clean(c.URL), IsPR: true})
	}
	r := resp.Repository
	if a := r.Node.AutoMerge; a != nil {
		d.AutoMerge, d.AutoBy = strings.ToLower(clean(a.Method)), clean(a.By.Login)
	}
	def := strings.ToLower(clean(r.Default))
	for _, m := range []struct {
		name string
		ok   bool
	}{{"squash", r.Squash}, {"merge", r.Merge}, {"rebase", r.Rebase}} {
		if !m.ok {
			continue
		}
		if m.name == def {
			d.Methods = append([]string{m.name}, d.Methods...)
		} else {
			d.Methods = append(d.Methods, m.name)
		}
	}
	return d, nil
}

func or0(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// checkRank puts failing checks first, then running ones, then the rest.
func checkRank(c Check) int {
	switch {
	case c.Status != "COMPLETED":
		return 1
	case c.Conclusion == "FAILURE" || c.Conclusion == "ERROR" || c.Conclusion == "TIMED_OUT" ||
		c.Conclusion == "ACTION_REQUIRED" || c.Conclusion == "STARTUP_FAILURE" || c.Conclusion == "CANCELLED":
		return 0
	}
	return 2
}

// ---- the diff ----

// File is one changed file of a pull request, with its patch.
type File struct {
	Path      string `json:"filename"`
	Previous  string `json:"previous_filename"`
	Status    string `json:"status"` // added, removed, modified, renamed, …
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// fetchFiles reads every changed file, a hundred to a page. GitHub lists at most 3000.
func fetchFiles(repo string, number int) ([]File, error) {
	var all []File
	for page := 1; page <= 30; page++ {
		var files []File
		if err := ghGet(fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100&page=%d", repo, number, page), &files); err != nil {
			return all, err
		}
		for i := range files {
			f := &files[i]
			f.Path, f.Previous, f.Status, f.Patch = clean(f.Path), clean(f.Previous), clean(f.Status), cleanKeepTabsLines(f.Patch)
		}
		all = append(all, files...)
		if len(files) < 100 {
			break
		}
	}
	return all, nil
}

// cleanKeepTabsLines cleans a patch line by line, keeping its newlines and tabs.
func cleanKeepTabsLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = cleanKeepTabs(l)
	}
	return strings.Join(lines, "\n")
}

// ReviewComment is a comment on a line of the diff, from REST, which gives the ids replies need.
type ReviewComment struct {
	ID        int64     `json:"id"`
	ReplyTo   int64     `json:"in_reply_to_id"`
	Path      string    `json:"path"`
	Line      int       `json:"line"`
	Side      string    `json:"side"`
	Original  int       `json:"original_line"`
	Body      string    `json:"body"`
	Created   time.Time `json:"created_at"`
	User      gqlLogin  `json:"user"`
	CommitID  string    `json:"commit_id"`
	StartLine int       `json:"start_line"` // the first line of a comment on several, else 0
	StartSide string    `json:"start_side"`
	Outdated  bool      `json:"-"`
	Pending   bool      `json:"-"` // a draft of yours, not yet sent
	DraftSide string    `json:"-"`
}

func fetchReviewComments(repo string, number int) ([]ReviewComment, error) {
	var all []ReviewComment
	for page := 1; page <= 10; page++ {
		var cs []ReviewComment
		if err := ghGet(fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100&page=%d", repo, number, page), &cs); err != nil {
			return all, err
		}
		for i := range cs {
			c := &cs[i]
			c.Path, c.Side, c.Body, c.User.Login = clean(c.Path), clean(c.Side), cleanBody(c.Body), clean(c.User.Login)
			// GitHub drops line when the code it was on has changed since.
			c.Outdated = c.Line == 0
		}
		all = append(all, cs...)
		if len(cs) < 100 {
			break
		}
	}
	return all, nil
}

// ---- writes ----

func postComment(repo string, number int, body string) error {
	return ghWrite("POST", fmt.Sprintf("repos/%s/issues/%d/comments", repo, number), map[string]string{"body": body}, nil)
}

func replyToComment(repo string, number int, id int64, body string) error {
	gh.forget(fmt.Sprintf("repos/%s/pulls/%d/comments", repo, number))
	return ghWrite("POST", fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", repo, number, id), map[string]string{"body": body}, nil)
}

// DraftComment is a line comment kept until the review it belongs to is submitted.
type DraftComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"` // RIGHT for the new code, LEFT for removed lines
	Body string `json:"body"`
}

// submitReview sends a review in one request: its verdict, its summary and every line comment drafted for it.
func submitReview(repo string, number int, sha, event, body string, comments []DraftComment) error {
	payload := map[string]any{"event": event, "commit_id": sha}
	if body != "" {
		payload["body"] = body
	}
	if len(comments) > 0 {
		payload["comments"] = comments
	}
	gh.forget(fmt.Sprintf("repos/%s/pulls/%d/comments", repo, number))
	return ghWrite("POST", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number), payload, nil)
}

func setState(repo string, number int, isPR bool, state string) error {
	kind := "issues"
	if isPR {
		kind = "pulls"
	}
	return ghWrite("PATCH", fmt.Sprintf("repos/%s/%s/%d", repo, kind, number), map[string]string{"state": state}, nil)
}

func mergePR(repo string, number int, sha, method string) error {
	return ghWrite("PUT", fmt.Sprintf("repos/%s/pulls/%d/merge", repo, number),
		map[string]string{"merge_method": method, "sha": sha}, nil)
}

func deleteBranch(repo, branch string) error {
	return ghWrite("DELETE", fmt.Sprintf("repos/%s/git/refs/heads/%s", repo, refPath(branch)), nil, nil)
}

// setAutoMerge has a pull request merge itself, by method, once its checks and reviews allow, if its head is still
// sha; or stops it.
func setAutoMerge(prID, method, sha string, on bool) error {
	if !on {
		return ghQuery(`mutation($id: ID!) { disablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`,
			map[string]any{"id": prID}, nil)
	}
	return ghQuery(`mutation($id: ID!, $m: PullRequestMergeMethod!, $sha: GitObjectID!) {
		enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $m, expectedHeadOid: $sha}) { clientMutationId } }`,
		map[string]any{"id": prID, "m": strings.ToUpper(method), "sha": sha}, nil)
}

func assign(repo string, number int, who string) error {
	return ghWrite("POST", fmt.Sprintf("repos/%s/issues/%d/assignees", repo, number), map[string][]string{"assignees": {who}}, nil)
}

// setDraft turns a pull request into a draft, or marks a draft ready for review.
func setDraft(id string, draft bool) error {
	m := `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { clientMutationId } }`
	if draft {
		m = `mutation($id: ID!) { convertPullRequestToDraft(input: {pullRequestId: $id}) { clientMutationId } }`
	}
	var out struct{}
	return ghQuery(m, map[string]any{"id": id}, &out)
}

// ---- Actions ----

type Run struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Branch     string    `json:"head_branch"`
	SHA        string    `json:"head_sha"`
	Event      string    `json:"event"`
	Title      string    `json:"display_title"`
	Created    time.Time `json:"created_at"`
	Updated    time.Time `json:"updated_at"`
	URL        string    `json:"html_url"`
	Actor      gqlLogin  `json:"actor"`
}

func fetchRuns(repo, branch string) ([]Run, error) {
	path := fmt.Sprintf("repos/%s/actions/runs?per_page=40", repo)
	if branch != "" {
		path += "&branch=" + url.QueryEscape(branch)
	}
	var resp struct {
		Runs []Run `json:"workflow_runs"`
	}
	err := ghGet(path, &resp)
	for i := range resp.Runs {
		r := &resp.Runs[i]
		r.Name, r.Status, r.Conclusion, r.Branch = clean(r.Name), clean(r.Status), clean(r.Conclusion), clean(r.Branch)
		r.SHA, r.Event, r.Title, r.URL, r.Actor.Login = clean(r.SHA), clean(r.Event), clean(r.Title), clean(r.URL), clean(r.Actor.Login)
	}
	return resp.Runs, err
}

// ---- new pull requests ----

type Branch struct {
	Name string
	SHA  string
	Date time.Time // the tip commit's
}

// BranchInfo is what a new pull request chooses from: the branches, and where recent pull requests went, which says
// how this repository works (straight to main, through develop, staging then main).
type BranchInfo struct {
	Default  string
	Branches []Branch // the most recently committed to first; at most 100
	Recent   []RecentPR
}

// RecentPR is one of the repository's latest pull requests, open or not: only which branch went where.
type RecentPR struct {
	Number     int
	Head, Base string
	Fork       bool
	Merged     bool
	HeadSHA    string // the head's commit when it was last updated, or merged
}

// usualBranches are the names teams give the branches work goes through. fetchBranches always looks for them, since a
// busy repository's hundred most recent branches can leave out master itself.
var usualBranches = []string{"main", "master", "develop", "development", "dev", "staging", "stage", "production", "prod",
	"release", "rc", "next", "qa", "uat"}

// fetchBranches lists the repository's branches, the most recently committed to first, then any usual ones that are
// older; the default; and the branches of its last 50 pull requests. One query.
func fetchBranches(repo string) (BranchInfo, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var usual strings.Builder
	for i, b := range usualBranches {
		fmt.Fprintf(&usual, "u%d: ref(qualifiedName: %q) { name target { oid ... on Commit { committedDate } } }\n", i, "refs/heads/"+b)
	}
	q := `query($o: String!, $n: String!) {
		repository(owner: $o, name: $n) {
			defaultBranchRef { name }
			refs(refPrefix: "refs/heads/", first: 100, orderBy: {field: TAG_COMMIT_DATE, direction: DESC}) {
				nodes { name target { oid ... on Commit { committedDate } } }
			}
			pullRequests(first: 50, orderBy: {field: CREATED_AT, direction: DESC}) {
				nodes { number headRefName baseRefName isCrossRepository state headRefOid }
			}
			` + usual.String() + `
		}
	}`
	var resp struct {
		Repository *struct {
			Default *struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
			Refs struct {
				Nodes []struct {
					Name   string `json:"name"`
					Target struct {
						OID  string    `json:"oid"`
						Date time.Time `json:"committedDate"`
					} `json:"target"`
				} `json:"nodes"`
			} `json:"refs"`
			PRs struct {
				Nodes []struct {
					Number int    `json:"number"`
					Head   string `json:"headRefName"`
					Base   string `json:"baseRefName"`
					Fork   bool   `json:"isCrossRepository"`
					State  string `json:"state"`
					OID    string `json:"headRefOid"`
				} `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	// The usual branches come back under aliases (u0, u1, …), so the answer is read twice: once for the fields the
	// struct names, once for the aliases.
	var data json.RawMessage
	if err := ghQuery(q, map[string]any{"o": owner, "n": name}, &data); err != nil {
		return BranchInfo{}, err
	}
	var raw struct {
		Repository map[string]json.RawMessage `json:"repository"`
	}
	if json.Unmarshal(data, &resp) != nil || json.Unmarshal(data, &raw) != nil || resp.Repository == nil {
		return BranchInfo{}, fmt.Errorf("GitHub has no repository %s that you can see", repo)
	}
	r := resp.Repository
	var info BranchInfo
	if r.Default != nil {
		info.Default = clean(r.Default.Name)
	}
	for _, n := range r.Refs.Nodes {
		info.Branches = append(info.Branches, Branch{Name: clean(n.Name), SHA: clean(n.Target.OID), Date: n.Target.Date})
	}
	listed := map[string]bool{}
	for _, b := range info.Branches {
		listed[b.Name] = true
	}
	for i := range usualBranches {
		var ref *struct {
			Name   string `json:"name"`
			Target struct {
				OID  string    `json:"oid"`
				Date time.Time `json:"committedDate"`
			} `json:"target"`
		}
		if b := raw.Repository[fmt.Sprintf("u%d", i)]; len(b) > 0 && json.Unmarshal(b, &ref) == nil && ref != nil && !listed[ref.Name] {
			info.Branches = append(info.Branches, Branch{Name: clean(ref.Name), SHA: clean(ref.Target.OID), Date: ref.Target.Date})
		}
	}
	for _, n := range r.PRs.Nodes {
		info.Recent = append(info.Recent, RecentPR{Number: n.Number, Head: clean(n.Head), Base: clean(n.Base), Fork: n.Fork,
			Merged: n.State == "MERGED", HeadSHA: clean(n.OID)})
	}
	return info, nil
}

// fetchHeadHistory finds the last pull requests from one branch, however long ago: a branch promoted once a month
// can be missing from the latest fifty.
func fetchHeadHistory(repo, head string) ([]RecentPR, error) {
	owner, name, _ := strings.Cut(repo, "/")
	q := `query($o: String!, $n: String!, $h: String!) {
		repository(owner: $o, name: $n) {
			pullRequests(headRefName: $h, first: 5, orderBy: {field: CREATED_AT, direction: DESC}) {
				nodes { number headRefName baseRefName isCrossRepository state headRefOid }
			}
		}
	}`
	var resp struct {
		Repository *struct {
			PRs struct {
				Nodes []struct {
					Number int    `json:"number"`
					Head   string `json:"headRefName"`
					Base   string `json:"baseRefName"`
					Fork   bool   `json:"isCrossRepository"`
					State  string `json:"state"`
					OID    string `json:"headRefOid"`
				} `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	if err := ghQuery(q, map[string]any{"o": owner, "n": name, "h": head}, &resp); err != nil || resp.Repository == nil {
		return nil, err
	}
	var out []RecentPR
	for _, n := range resp.Repository.PRs.Nodes {
		out = append(out, RecentPR{Number: n.Number, Head: clean(n.Head), Base: clean(n.Base), Fork: n.Fork,
			Merged: n.State == "MERGED", HeadSHA: clean(n.OID)})
	}
	return out, nil
}

// fetchBranch looks up one branch, for one past the hundred fetchBranches lists. ok is false when GitHub has none.
func fetchBranch(repo, name string) (b Branch, ok bool, err error) {
	var resp struct {
		Name   string `json:"name"`
		Commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Committer struct {
					Date time.Time `json:"date"`
				} `json:"committer"`
			} `json:"commit"`
		} `json:"commit"`
	}
	if err := ghGet(fmt.Sprintf("repos/%s/branches/%s", repo, refPath(name)), &resp); err != nil {
		if strings.HasPrefix(err.Error(), "GitHub: 404") {
			return Branch{}, false, nil
		}
		return Branch{}, false, err
	}
	return Branch{Name: clean(resp.Name), SHA: clean(resp.Commit.SHA), Date: resp.Commit.Commit.Committer.Date}, true, nil
}

// Comparison is what merging head into base would bring.
type Comparison struct {
	Status             string // ahead, behind, diverged, identical
	Ahead, Behind      int
	Commits            []Commit // the oldest first, as GitHub lists them; at most 250
	Files              int
	Additions, Removed int
	Open               *Item // an open pull request from head into base already
}

type Commit struct {
	SHA, Subject, Body, Author string
	Parents                    []string
}

// fetchComparison compares two branches, and looks for an open pull request between them.
func fetchComparison(repo, base, head string) (*Comparison, error) {
	var resp struct {
		Status  string `json:"status"`
		Ahead   int    `json:"ahead_by"`
		Behind  int    `json:"behind_by"`
		Commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"commit"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		} `json:"commits"`
		Files []struct {
			Additions int `json:"additions"`
			Deletions int `json:"deletions"`
		} `json:"files"`
	}
	if err := ghGet(fmt.Sprintf("repos/%s/compare/%s...%s", repo, refPath(base), refPath(head)), &resp); err != nil {
		return nil, err
	}
	c := &Comparison{Status: clean(resp.Status), Ahead: resp.Ahead, Behind: resp.Behind, Files: len(resp.Files)}
	for _, f := range resp.Files {
		c.Additions += f.Additions
		c.Removed += f.Deletions
	}
	for _, rc := range resp.Commits {
		subject, body, _ := strings.Cut(cleanBody(rc.Commit.Message), "\n")
		cm := Commit{SHA: clean(rc.SHA), Subject: strings.TrimSpace(subject), Body: strings.TrimSpace(body),
			Author: clean(rc.Commit.Author.Name)}
		for _, p := range rc.Parents {
			cm.Parents = append(cm.Parents, clean(p.SHA))
		}
		c.Commits = append(c.Commits, cm)
	}
	owner, _, _ := strings.Cut(repo, "/")
	var open []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"html_url"`
	}
	path := fmt.Sprintf("repos/%s/pulls?state=open&base=%s&head=%s", repo, url.QueryEscape(base), url.QueryEscape(owner+":"+head))
	if err := ghGet(path, &open); err != nil {
		return nil, err
	}
	if len(open) > 0 {
		o := open[0]
		c.Open = &Item{Number: o.Number, Title: clean(o.Title), URL: clean(o.URL), IsPR: true, State: "OPEN", Head: head, Base: base}
	}
	return c, nil
}

// refPath escapes a branch name for a URL path, keeping the slashes that branch names use as separators.
func refPath(branch string) string {
	parts := strings.Split(branch, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// createPR opens a pull request from head into base, both branches of this repository.
func createPR(repo, base, head, title, body string, draft bool) (Item, error) {
	var resp struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"html_url"`
	}
	payload := map[string]any{"base": base, "head": head, "title": title, "body": body, "draft": draft}
	if err := ghWrite("POST", fmt.Sprintf("repos/%s/pulls", repo), payload, &resp); err != nil {
		return Item{}, err
	}
	return Item{Number: resp.Number, Title: clean(resp.Title), URL: clean(resp.URL), IsPR: true, State: "OPEN",
		Draft: draft, Head: head, Base: base}, nil
}

// ---- review state ----

// ReviewState is what the diff needs beyond REST: each thread's node id and whether it is resolved, keyed by its first
// comment's id; the files you have marked viewed; and the commit your last review was on.
type ReviewState struct {
	PRID       string
	Threads    map[int64]ThreadState
	Viewed     map[string]bool
	LastReview string // the head commit when you last reviewed, or ""
}

type ThreadState struct {
	ID       string
	Resolved bool
}

// fetchReviewState reads it in one query, paging the files a hundred at a time.
func fetchReviewState(repo string, number int) (*ReviewState, error) {
	owner, name, _ := strings.Cut(repo, "/")
	q := `query($o: String!, $n: String!, $num: Int!, $after: String) {
		repository(owner: $o, name: $n) {
			pullRequest(number: $num) {
				id
				viewerLatestReview { commit { oid } }
				files(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { path viewerViewedState } }
				reviewThreads(first: 100) { pageInfo { hasNextPage endCursor } nodes { id isResolved comments(first: 1) { nodes { fullDatabaseId } } } }
			}
		}
	}`
	st := &ReviewState{Threads: map[int64]ThreadState{}, Viewed: map[string]bool{}}
	var after *string
	for page := 0; page < 30; page++ {
		var resp struct {
			Repository *struct {
				PR *struct {
					ID     string `json:"id"`
					Latest *struct {
						Commit *struct {
							OID string `json:"oid"`
						} `json:"commit"`
					} `json:"viewerLatestReview"`
					Files struct {
						PageInfo struct {
							Next   bool   `json:"hasNextPage"`
							Cursor string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							Path   string `json:"path"`
							Viewed string `json:"viewerViewedState"`
						} `json:"nodes"`
					} `json:"files"`
					Threads gqlThreads `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := ghQuery(q, map[string]any{"o": owner, "n": name, "num": number, "after": after}, &resp); err != nil {
			return nil, err
		}
		if resp.Repository == nil || resp.Repository.PR == nil {
			return nil, fmt.Errorf("GitHub has no pull request #%d in %s", number, repo)
		}
		pr := resp.Repository.PR
		st.PRID = pr.ID
		if pr.Latest != nil && pr.Latest.Commit != nil {
			st.LastReview = clean(pr.Latest.Commit.OID)
		}
		for _, f := range pr.Files.Nodes {
			st.Viewed[clean(f.Path)] = f.Viewed == "VIEWED"
		}
		if page == 0 {
			st.add(pr.Threads)
			if pr.Threads.PageInfo.Next {
				if err := st.moreThreads(owner, name, number, pr.Threads.PageInfo.Cursor); err != nil {
					return nil, err
				}
			}
		}
		if !pr.Files.PageInfo.Next {
			break
		}
		c := pr.Files.PageInfo.Cursor
		after = &c
	}
	return st, nil
}

// gqlThreads is a page of review threads, each known by its first comment's id: the id REST gives the comment.
type gqlThreads struct {
	PageInfo struct {
		Next   bool   `json:"hasNextPage"`
		Cursor string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []struct {
		ID       string `json:"id"`
		Resolved bool   `json:"isResolved"`
		Comments struct {
			Nodes []struct {
				ID string `json:"fullDatabaseId"` // a BigInt, as a string: the 32-bit databaseId runs out
			} `json:"nodes"`
		} `json:"comments"`
	} `json:"nodes"`
}

func (st *ReviewState) add(page gqlThreads) {
	for _, t := range page.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		if id, err := strconv.ParseInt(t.Comments.Nodes[0].ID, 10, 64); err == nil {
			st.Threads[id] = ThreadState{ID: clean(t.ID), Resolved: t.Resolved}
		}
	}
}

// moreThreads reads the threads past the first hundred, a hundred at a time.
func (st *ReviewState) moreThreads(owner, name string, number int, after string) error {
	q := `query($o: String!, $n: String!, $num: Int!, $after: String) {
		repository(owner: $o, name: $n) { pullRequest(number: $num) {
			reviewThreads(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { id isResolved comments(first: 1) { nodes { fullDatabaseId } } } }
		} }
	}`
	for page := 0; page < 20 && after != ""; page++ {
		var resp struct {
			Repository *struct {
				PR *struct {
					Threads gqlThreads `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := ghQuery(q, map[string]any{"o": owner, "n": name, "num": number, "after": after}, &resp); err != nil {
			return err
		}
		if resp.Repository == nil || resp.Repository.PR == nil {
			return nil
		}
		t := resp.Repository.PR.Threads
		st.add(t)
		after = ""
		if t.PageInfo.Next {
			after = t.PageInfo.Cursor
		}
	}
	return nil
}

// setResolved resolves a review thread, or opens it again.
func setResolved(threadID string, resolved bool) error {
	m := "unresolveReviewThread"
	if resolved {
		m = "resolveReviewThread"
	}
	return ghQuery(`mutation($id: ID!) { `+m+`(input: {threadId: $id}) { thread { id } } }`, map[string]any{"id": threadID}, nil)
}

// setViewed ticks a file's "Viewed" box on GitHub, or clears it.
func setViewed(prID, path string, viewed bool) error {
	m := "unmarkFileAsViewed"
	if viewed {
		m = "markFileAsViewed"
	}
	return ghQuery(`mutation($id: ID!, $path: String!) { `+m+`(input: {pullRequestId: $id, path: $path}) { clientMutationId } }`,
		map[string]any{"id": prID, "path": path}, nil)
}

// fetchFilesSince is what changed between two commits, as files with patches: a pull request since you reviewed it.
func fetchFilesSince(repo, from, to string) ([]File, error) {
	var resp struct {
		Files []File `json:"files"`
	}
	if err := ghGet(fmt.Sprintf("repos/%s/compare/%s...%s", repo, from, to), &resp); err != nil {
		return nil, err
	}
	for i := range resp.Files {
		f := &resp.Files[i]
		f.Path, f.Previous, f.Status, f.Patch = clean(f.Path), clean(f.Previous), clean(f.Status), cleanKeepTabsLines(f.Patch)
	}
	return resp.Files, nil
}

// ---- reviewers and labels ----

// Reviewer is someone, or a team, who could be asked to review.
type Reviewer struct {
	Login     string // a user's login, or a team's slug
	Name      string
	Team      bool
	Requested bool   // asked already
	Why       string // GitHub's reason to suggest them, when it does
}

// key names a reviewer across users and teams.
func (r Reviewer) key() string {
	if r.Team {
		return "team:" + r.Login
	}
	return r.Login
}

// fetchReviewers lists who could review a pull request: those asked already, GitHub's suggestions (people who
// changed this code before), the organization's teams when the token can see them, and everyone who can be assigned.
func fetchReviewers(repo string, number int) ([]Reviewer, error) {
	owner, name, _ := strings.Cut(repo, "/")
	q := `query($o: String!, $n: String!, $num: Int!) {
		repository(owner: $o, name: $n) {
			pullRequest(number: $num) {
				author { login }
				reviewRequests(first: 50) { nodes { requestedReviewer { __typename ... on User { login name } ... on Team { slug name } } } }
				suggestedReviewers { isAuthor isCommenter reviewer { login name } }
			}
			assignableUsers(first: 100) { nodes { login name } }
		}
	}`
	type person struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
		Slug     string `json:"slug"`
		Name     string `json:"name"`
	}
	var resp struct {
		Repository *struct {
			PR *struct {
				Author   gqlLogin `json:"author"`
				Requests struct {
					Nodes []struct {
						Who person `json:"requestedReviewer"`
					} `json:"nodes"`
				} `json:"reviewRequests"`
				Suggested []struct {
					Author    bool   `json:"isAuthor"`
					Commenter bool   `json:"isCommenter"`
					Who       person `json:"reviewer"`
				} `json:"suggestedReviewers"`
			} `json:"pullRequest"`
			Assignable struct {
				Nodes []person `json:"nodes"`
			} `json:"assignableUsers"`
		} `json:"repository"`
	}
	if err := ghQuery(q, map[string]any{"o": owner, "n": name, "num": number}, &resp); err != nil {
		return nil, err
	}
	if resp.Repository == nil || resp.Repository.PR == nil {
		return nil, fmt.Errorf("GitHub has no pull request #%d in %s", number, repo)
	}
	pr := resp.Repository.PR
	author := clean(pr.Author.Login)
	var out []Reviewer
	seen := map[string]bool{}
	add := func(r Reviewer) {
		if r.Login == "" || seen[r.key()] || !r.Team && r.Login == author {
			return
		}
		seen[r.key()] = true
		out = append(out, r)
	}
	for _, n := range pr.Requests.Nodes {
		if n.Who.Typename == "Team" {
			add(Reviewer{Login: clean(n.Who.Slug), Name: clean(n.Who.Name), Team: true, Requested: true})
		} else {
			add(Reviewer{Login: clean(n.Who.Login), Name: clean(n.Who.Name), Requested: true})
		}
	}
	for _, s := range pr.Suggested {
		why := "changed this code before"
		if s.Commenter {
			why = "commented on this pull request"
		}
		add(Reviewer{Login: clean(s.Who.Login), Name: clean(s.Who.Name), Why: why})
	}
	// Teams need the organization's permission to read them; without it, people are enough.
	var teams struct {
		Organization *struct {
			Teams struct {
				Nodes []person `json:"nodes"`
			} `json:"teams"`
		} `json:"organization"`
	}
	if ghQuery(`query($o: String!) { organization(login: $o) { teams(first: 50, orderBy: {field: NAME, direction: ASC}) { nodes { slug name } } } }`,
		map[string]any{"o": owner}, &teams) == nil && teams.Organization != nil {
		for _, t := range teams.Organization.Teams.Nodes {
			add(Reviewer{Login: clean(t.Slug), Name: clean(t.Name), Team: true})
		}
	}
	for _, p := range resp.Repository.Assignable.Nodes {
		add(Reviewer{Login: clean(p.Login), Name: clean(p.Name)})
	}
	return out, nil
}

// setReviewers asks the chosen reviewers and stops asking the ones no longer chosen.
func setReviewers(repo string, number int, add, remove []Reviewer) error {
	split := func(rs []Reviewer) map[string]any {
		users, teams := []string{}, []string{}
		for _, r := range rs {
			if r.Team {
				teams = append(teams, r.Login)
			} else {
				users = append(users, r.Login)
			}
		}
		return map[string]any{"reviewers": users, "team_reviewers": teams}
	}
	path := fmt.Sprintf("repos/%s/pulls/%d/requested_reviewers", repo, number)
	if len(remove) > 0 {
		if err := ghWrite("DELETE", path, split(remove), nil); err != nil {
			return err
		}
	}
	if len(add) > 0 {
		return ghWrite("POST", path, split(add), nil)
	}
	return nil
}

type Label struct {
	Name, Description string
}

// fetchLabels lists the repository's labels, by name.
func fetchLabels(repo string) ([]Label, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var resp struct {
		Repository *struct {
			Labels struct {
				Nodes []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"nodes"`
			} `json:"labels"`
		} `json:"repository"`
	}
	if err := ghQuery(`query($o: String!, $n: String!) { repository(owner: $o, name: $n) {
		labels(first: 100, orderBy: {field: NAME, direction: ASC}) { nodes { name description } } } }`,
		map[string]any{"o": owner, "n": name}, &resp); err != nil {
		return nil, err
	}
	if resp.Repository == nil {
		return nil, fmt.Errorf("GitHub has no repository %s that you can see", repo)
	}
	var out []Label
	for _, l := range resp.Repository.Labels.Nodes {
		out = append(out, Label{Name: clean(l.Name), Description: clean(l.Description)})
	}
	return out, nil
}

// setLabels replaces a pull request's or issue's labels with these.
func setLabels(repo string, number int, labels []string) error {
	if labels == nil {
		labels = []string{}
	}
	return ghWrite("PUT", fmt.Sprintf("repos/%s/issues/%d/labels", repo, number), map[string]any{"labels": labels}, nil)
}

// ---- inbox ----

// Notification is one thread in your GitHub inbox, from any repository.
type Notification struct {
	ID      string
	Repo    string // owner/name
	Kind    string // PullRequest, Issue, Release, Discussion, CheckSuite, Commit, …
	Title   string
	Reason  string // review_requested, mention, assign, author, comment, ci_activity, …
	Unread  bool
	Updated time.Time
	Number  int    // the pull request's or issue's, when it is one
	URL     string // on github.com
}

var subjectNumber = regexp.MustCompile(`/(pulls|issues)/(\d+)$`)

// fetchNotifications reads your inbox, unread only unless all. GitHub answers an unchanged inbox with 304, which
// costs nothing.
func fetchNotifications(all bool) ([]Notification, error) {
	var resp []struct {
		ID      string    `json:"id"`
		Reason  string    `json:"reason"`
		Unread  bool      `json:"unread"`
		Updated time.Time `json:"updated_at"`
		Subject struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Type  string `json:"type"`
		} `json:"subject"`
		Repository struct {
			FullName string `json:"full_name"`
			URL      string `json:"html_url"`
		} `json:"repository"`
	}
	if err := ghGet(fmt.Sprintf("notifications?all=%t&per_page=50", all), &resp); err != nil {
		return nil, err
	}
	var out []Notification
	for _, r := range resp {
		n := Notification{ID: clean(r.ID), Repo: clean(r.Repository.FullName), Kind: clean(r.Subject.Type),
			Title: clean(r.Subject.Title), Reason: clean(r.Reason), Unread: r.Unread, Updated: r.Updated,
			URL: clean(r.Repository.URL)}
		if m := subjectNumber.FindStringSubmatch(r.Subject.URL); m != nil {
			n.Number, _ = strconv.Atoi(m[2])
			path := "issues"
			if n.Kind == "PullRequest" {
				path = "pull"
			}
			n.URL = fmt.Sprintf("%s/%s/%d", n.URL, path, n.Number)
		} else if n.Kind == "CheckSuite" {
			n.URL += "/actions"
		}
		out = append(out, n)
	}
	return out, nil
}

// markRead marks a notification read; markDone takes it out of the inbox.
func markRead(id string) error { return inboxWrite("PATCH", id) }
func markDone(id string) error { return inboxWrite("DELETE", id) }

func inboxWrite(method, id string) error {
	err := ghWrite(method, "notifications/threads/"+id, nil, nil)
	gh.forget("notifications") // the inbox changed: ask again, not from the last answer
	return err
}
