package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ghdeck reads GitHub through its APIs rather than one `gh` process per question. REST responses are kept with their
// ETag and asked for again with If-None-Match: GitHub answers 304 when nothing changed, and a 304 does not count against
// the hourly rate limit, so polling a quiet repo costs next to nothing. Questions asked at the same moment share one
// request. Lists and detail screens come from GraphQL, one query each, which has its own hourly budget of points. When
// either limit is reached, ghdeck stops asking until GitHub says it resets, instead of failing every few seconds and
// making it worse. The token is gh's own (`gh auth token`) and is only ever sent to the API host.

const (
	ghFresh      = 3 * time.Second  // an answer this new is reused without asking again
	ghLowFresh   = time.Minute      // the same, while the hourly budget is running low
	ghLowBudget  = 1000             // left that count as running low: gh and agents share the same budget
	ghMaxBody    = 16 << 20         // the largest response read
	ghMaxEntries = 400              // responses kept for revalidation
	ghTimeout    = 20 * time.Second // one request
)

type ghEntry struct {
	etag string
	body []byte
	at   time.Time
}

type ghFlight struct {
	done chan struct{}
	body []byte
	err  error
}

type ghClient struct {
	base    string
	graphql string
	http    *http.Client

	mu        sync.Mutex
	token     string
	cache     map[string]*ghEntry
	inflight  map[string]*ghFlight
	limit     int
	remaining int // REST requests left this hour
	reset     time.Time
	gqlLeft   int // GraphQL points left this hour
	gqlReset  time.Time
	paused    time.Time // no requests before this
}

var gh = newGHClient()

func newGHClient() *ghClient {
	base, graphql := "https://api.github.com", "https://api.github.com/graphql"
	if h := os.Getenv("GH_HOST"); h != "" && h != "github.com" {
		base, graphql = "https://"+h+"/api/v3", "https://"+h+"/api/graphql"
	}
	return &ghClient{
		base:      base,
		graphql:   graphql,
		http:      &http.Client{Timeout: ghTimeout},
		cache:     map[string]*ghEntry{},
		inflight:  map[string]*ghFlight{},
		remaining: -1,
		gqlLeft:   -1,
	}
}

// rateLimitError says GitHub's limit was reached and when ghdeck will ask again.
type rateLimitError struct{ until time.Time }

func (e rateLimitError) Error() string {
	return "GitHub rate limit reached; ghdeck pauses GitHub until " + e.until.Local().Format("15:04")
}

// ghGet fetches an API path (no leading slash) and decodes it into v.
func ghGet(path string, v any) error {
	body, err := gh.get(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func (c *ghClient) get(path string) ([]byte, error) {
	c.mu.Lock()
	e := c.cache[path]
	if e != nil {
		fresh := ghFresh
		if c.remaining >= 0 && c.remaining < ghLowBudget {
			fresh = ghLowFresh
		}
		if time.Since(e.at) < fresh {
			c.mu.Unlock()
			return e.body, nil
		}
	}
	if time.Now().Before(c.paused) {
		until := c.paused
		c.mu.Unlock()
		return nil, rateLimitError{until}
	}
	if f := c.inflight[path]; f != nil {
		c.mu.Unlock()
		<-f.done
		return f.body, f.err
	}
	f := &ghFlight{done: make(chan struct{})}
	c.inflight[path] = f
	c.mu.Unlock()

	f.body, f.err = c.fetch(path, e)

	c.mu.Lock()
	delete(c.inflight, path)
	c.mu.Unlock()
	close(f.done)
	return f.body, f.err
}

func (c *ghClient) fetch(path string, cached *ghEntry) ([]byte, error) {
	token, err := c.authToken(false)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(path, token, cached)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		// The token may have been refreshed by `gh auth login` since ghdeck read it.
		resp.Body.Close()
		if token, err = c.authToken(true); err != nil {
			return nil, err
		}
		resp, err = c.do(path, token, cached)
	}
	if err != nil {
		return nil, fmt.Errorf("GitHub: %s", errText(err))
	}
	defer resp.Body.Close()
	c.noteLimits(resp)

	switch {
	case resp.StatusCode == http.StatusNotModified && cached != nil:
		c.mu.Lock()
		cached.at = time.Now()
		c.mu.Unlock()
		return cached.body, nil
	case resp.StatusCode == http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, ghMaxBody))
		if err != nil {
			return nil, fmt.Errorf("GitHub: %s", errText(err))
		}
		c.store(path, resp.Header.Get("ETag"), body)
		return body, nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var apiErr struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &apiErr)
	msg := clean(apiErr.Message)
	if until, limited := c.limitedUntil(resp, msg); limited {
		return nil, rateLimitError{until}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("gh: %w", errAuth)
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return nil, fmt.Errorf("GitHub: %d %s", resp.StatusCode, msg)
}

func (c *ghClient) do(path, token string, cached *ghEntry) (*http.Response, error) {
	req, err := http.NewRequest("GET", c.base+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ghdeck")
	if cached != nil && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}
	return c.http.Do(req)
}

// authToken asks gh for its token once, and again when GitHub refuses it.
func (c *ghClient) authToken(renew bool) (string, error) {
	c.mu.Lock()
	t := c.token
	c.mu.Unlock()
	if t != "" && !renew {
		return t, nil
	}
	out, err := run("gh", "auth", "token")
	if err != nil {
		if errors.Is(err, errAuth) || strings.Contains(err.Error(), "not logged") {
			return "", fmt.Errorf("gh: %w", errAuth)
		}
		return "", err
	}
	t = strings.TrimSpace(string(out))
	if t == "" {
		return "", fmt.Errorf("gh: %w", errAuth)
	}
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
	return t, nil
}

func (c *ghClient) store(path, etag string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= ghMaxEntries {
		var oldest string
		for k, e := range c.cache {
			if oldest == "" || e.at.Before(c.cache[oldest].at) {
				oldest = k
			}
		}
		delete(c.cache, oldest)
	}
	c.cache[path] = &ghEntry{etag: etag, body: body, at: time.Now()}
}

func (c *ghClient) noteLimits(resp *http.Response) {
	rem, err1 := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	lim, err2 := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	reset, err3 := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	c.mu.Lock()
	defer c.mu.Unlock()
	if resp.Header.Get("X-RateLimit-Resource") == "graphql" {
		if err1 == nil {
			c.gqlLeft = rem
		}
		if err3 == nil {
			c.gqlReset = time.Unix(reset, 0)
		}
		return
	}
	if err1 == nil {
		c.remaining = rem
	}
	if err2 == nil {
		c.limit = lim
	}
	if err3 == nil {
		c.reset = time.Unix(reset, 0)
	}
}

// limitedUntil recognizes both of GitHub's limits: the hourly one (no requests left, wait for the reset) and the
// secondary one for bursts (Retry-After, or a minute when it does not say).
func (c *ghClient) limitedUntil(resp *http.Response, msg string) (time.Time, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var until time.Time
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		until = time.Now().Add(time.Duration(s) * time.Second)
	} else if c.remaining == 0 && c.reset.After(time.Now()) {
		until = c.reset
	} else if strings.Contains(strings.ToLower(msg), "rate limit") {
		until = time.Now().Add(time.Minute)
	} else {
		return time.Time{}, false
	}
	// A moment past the reset, so the first request after it is not refused again.
	until = until.Add(2 * time.Second)
	if until.After(c.paused) {
		c.paused = until
	}
	return c.paused, true
}

// ghBudget describes the hourly budget for the footer: empty while it is healthy.
func ghBudget() string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	switch {
	case time.Now().Before(gh.paused):
		return "GitHub paused until " + gh.paused.Local().Format("15:04") + " (rate limit)"
	case gh.remaining >= 0 && gh.remaining < ghLowBudget:
		return fmt.Sprintf("GitHub: %d requests left until %s, refreshing less often", gh.remaining, gh.reset.Local().Format("15:04"))
	case gh.gqlLeft >= 0 && gh.gqlLeft < ghLowBudget:
		return fmt.Sprintf("GitHub: %d GraphQL points left until %s, refreshing less often", gh.gqlLeft, gh.gqlReset.Local().Format("15:04"))
	}
	return ""
}

// lowBudget says either hourly budget is running low, so lists refresh less often.
func lowBudget() bool {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return gh.remaining >= 0 && gh.remaining < ghLowBudget || gh.gqlLeft >= 0 && gh.gqlLeft < ghLowBudget
}

// Nobody needs a list kept fresh to the second in a pane they stopped looking at, and a deck left open in a few Herdr
// workspaces all day would otherwise spend the hourly budget that gh and the agents share with it. After a while with
// no keys pressed, every refresh slows down; the next key refreshes everything and goes back to the usual pace. (Herdr
// tells panes they have focus when they have not, so focus does not count.) Only the UI goroutine touches lastActive.
const (
	idleAfter   = 10 * time.Minute
	idleEvery   = 2 * time.Minute
	asleepAfter = time.Hour
	asleepEvery = 10 * time.Minute
)

var lastActive = time.Now()

// idle says nobody has used ghdeck for a while.
func idle() bool { return time.Since(lastActive) > idleAfter }

// pace stretches a refresh interval while ghdeck sits idle or the hourly budget runs low.
func pace(every time.Duration) time.Duration {
	switch quiet := time.Since(lastActive); {
	case quiet > asleepAfter:
		every = max(every, asleepEvery)
	case quiet > idleAfter:
		every = max(every, idleEvery)
	}
	if lowBudget() {
		every = max(every, 2*time.Minute)
	}
	return every
}

// forget drops cached responses under a path prefix, after a write that changes them.
func (c *ghClient) forget(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.cache {
		if strings.HasPrefix(k, prefix) {
			delete(c.cache, k)
		}
	}
}

// ghQuery runs a GraphQL query and decodes its data into v. GraphQL has no ETags, so nothing here is cached; callers
// poll it on a slower schedule than REST.
func ghQuery(query string, vars map[string]any, v any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	mutation := strings.HasPrefix(strings.TrimSpace(query), "mutation")
	key := ""
	var out []byte
	if !mutation {
		key = sharedKey(body)
		out = sharedGet(key)
	}
	fresh := out == nil
	if fresh {
		if out, err = gh.send("POST", gh.graphql, body); err != nil {
			return err
		}
	}
	if mutation {
		sharedClear()
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return fmt.Errorf("GitHub: %s", errText(err))
	}
	if len(resp.Errors) > 0 {
		e := resp.Errors[0]
		if e.Type == "RATE_LIMITED" {
			gh.mu.Lock()
			until := gh.gqlReset
			if until.Before(time.Now()) {
				until = time.Now().Add(time.Minute)
			}
			gh.paused = until.Add(2 * time.Second)
			gh.mu.Unlock()
			return rateLimitError{until}
		}
		return fmt.Errorf("GitHub: %s", clean(e.Message))
	}
	if fresh && key != "" {
		sharedPut(key, out)
	}
	if v == nil { // a mutation whose answer nothing reads
		return nil
	}
	return json.Unmarshal(resp.Data, v)
}

// ghWrite sends a REST write (POST, PATCH, PUT, DELETE) to an API path and decodes the reply into v, if v is not nil.
func ghWrite(method, path string, payload, v any) error {
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	out, err := gh.send(method, gh.base+"/"+path, body)
	sharedClear() // whatever changed, every deck should see it
	if err != nil {
		return err
	}
	if v == nil || len(out) == 0 {
		return nil
	}
	return json.Unmarshal(out, v)
}

// send makes one uncached request to a full URL: a GraphQL query or a REST write.
func (c *ghClient) send(method, url string, body []byte) ([]byte, error) {
	c.mu.Lock()
	if time.Now().Before(c.paused) {
		until := c.paused
		c.mu.Unlock()
		return nil, rateLimitError{until}
	}
	c.mu.Unlock()
	token, err := c.authToken(false)
	if err != nil {
		return nil, err
	}
	do := func(token string) (*http.Response, error) {
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "ghdeck")
		req.Header.Set("Content-Type", "application/json")
		return c.http.Do(req)
	}
	resp, err := do(token)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if token, err = c.authToken(true); err != nil {
			return nil, err
		}
		resp, err = do(token)
	}
	if err != nil {
		return nil, fmt.Errorf("GitHub: %s", errText(err))
	}
	defer resp.Body.Close()
	c.noteLimits(resp)
	out, err := io.ReadAll(io.LimitReader(resp.Body, ghMaxBody))
	if err != nil {
		return nil, fmt.Errorf("GitHub: %s", errText(err))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return out, nil
	}
	var apiErr struct {
		Message string `json:"message"`
		Errors  []any  `json:"errors"`
	}
	_ = json.Unmarshal(out, &apiErr)
	msg := clean(apiErr.Message)
	if until, limited := c.limitedUntil(resp, msg); limited {
		return nil, rateLimitError{until}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("gh: %w", errAuth)
	}
	if len(apiErr.Errors) > 0 {
		detail, _ := json.Marshal(apiErr.Errors[0])
		msg += ": " + clean(truncateBytes(string(detail), 200))
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return nil, fmt.Errorf("GitHub: %d %s", resp.StatusCode, msg)
}
