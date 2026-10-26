package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"
)

// GraphQL answers are shared between every ghdeck you have open, through files: two decks on the same repository, in
// two Herdr panes, ask GitHub once between them instead of once each. GraphQL has no ETags, so without this every
// open deck pays for every refresh. Herdr plugins may not leave a process running in the background, so there is no
// shared poller; the decks that are open share what they fetch instead.
//
// An answer is reused for sharedFresh, less than the fastest refresh, so nothing shows older data than it would have
// fetched itself. Answers are keyed by the token as well as the question, so a different gh account never sees
// another's. Any write clears them all, so no deck goes on showing what was there before the change. The files are
// yours alone (0600, in your cache directory) and are swept after an hour.

const sharedFresh = 20 * time.Second

func sharedDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "ghdeck", "answers")
}

// sharedKey names an answer by the API, the account and the question.
func sharedKey(body []byte) string {
	gh.mu.Lock()
	token := gh.token
	gh.mu.Unlock()
	if token == "" {
		return "" // not signed in yet: nothing to share
	}
	h := sha256.New()
	h.Write([]byte(gh.graphql + "\x00" + token + "\x00"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// sharedGet returns another deck's answer, when there is a fresh one.
func sharedGet(key string) []byte {
	dir := sharedDir()
	if key == "" || dir == "" {
		return nil
	}
	path := filepath.Join(dir, key)
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > sharedFresh || info.Size() > ghMaxBody {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// sharedPut keeps an answer for the other decks. A file is written whole, then renamed, so nobody reads half of one.
func sharedPut(key string, body []byte) {
	dir := sharedDir()
	if key == "" || dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	_, werr := f.Write(body)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Rename(f.Name(), filepath.Join(dir, key)) != nil {
		os.Remove(f.Name())
	}
}

// sharedClear forgets every shared answer, after a write.
func sharedClear() {
	if dir := sharedDir(); dir != "" {
		os.RemoveAll(dir)
	}
}

// sharedSweep removes answers nobody has used for an hour; ghdeck runs it once at start.
func sharedSweep() {
	dir := sharedDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
