package main

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Anything ghdeck shows that it did not write itself (titles, descriptions, comments, diffs, branch names, logs, error
// text) passes through clean first. Anyone who can open an issue on a public repo can write those, and a terminal acts
// on control sequences in what it draws: one hidden in a comment could set the clipboard (OSC 52), retitle the window
// or redraw the screen.

var escapeSequence = regexp.MustCompile(
	// OSC, DCS, SOS, PM and APC strings, ended by BEL or ST (ESC \ or the C1 byte), or left unterminated.
	"\x1b[\\]PX^_][^\x07\x1b\u009c]*(?:\x07|\x1b\\\\|\u009c)?" +
		// CSI, from ESC [ or the C1 CSI.
		"|(?:\x1b\\[|\u009b)[0-?]*[ -/]*[@-~]" +
		// Any other escape: ESC, optional intermediates, a final byte.
		"|\x1b[ -/]*[0-~]?")

// clean removes escape sequences and control characters, turning tabs into spaces. Invalid UTF-8 becomes U+FFFD, so
// a lone C1 byte cannot reach the terminal either, and bidirectional overrides, which can make text read as something
// it is not, are dropped.
func clean(s string) string { return sanitize(s, false) }

// cleanKeepTabs is clean for raw output that is split on tabs before it is shown (gh's job logs).
func cleanKeepTabs(s string) string { return sanitize(s, true) }

func sanitize(s string, keepTabs bool) string {
	if isPlain(s) {
		return s
	}
	s = strings.ToValidUTF8(s, "\ufffd")
	s = escapeSequence.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' && keepTabs:
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0xfeff:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// forEditing is text as it was written, for the editor to start from, so saving changes only what you changed: tabs,
// bidirectional marks and the rest stay. Only what could act on the terminal goes (escape sequences, and control
// characters other than tab and newline), and text with none of that comes through untouched.
func forEditing(s string) string {
	s = strings.ToValidUTF8(s, "\ufffd")
	s = escapeSequence.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t', r == '\n':
			b.WriteRune(r)
		case r == '\r', r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isPlain is the fast path: printable ASCII needs no work.
func isPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return clean(err.Error())
}

func cleanJobs(jobs []jobDetail) {
	for i := range jobs {
		j := &jobs[i]
		j.Name, j.Status, j.Conclusion, j.URL = clean(j.Name), clean(j.Status), clean(j.Conclusion), clean(j.URL)
		for k := range j.Steps {
			s := &j.Steps[k]
			s.Name, s.Status, s.Conclusion = clean(s.Name), clean(s.Status), clean(s.Conclusion)
		}
	}
}

// truncateBytes keeps at most n bytes without splitting a character.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
