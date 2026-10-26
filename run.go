package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// errAuth means gh needs an interactive login before anything else will work.
var errAuth = errors.New("login expired")

const cmdTimeout = 45 * time.Second

func run(name string, args ...string) ([]byte, error) {
	return runIn("", name, args...)
}

func runIn(dir, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var notFound *exec.Error
		if errors.As(err, &notFound) {
			return nil, fmt.Errorf("%s is not installed", name)
		}
		return nil, cliError(name, stderr.String(), err)
	}
	return out, nil
}

// cliError turns a CLI's stderr into one readable line, recognizing expired logins.
func cliError(name, stderr string, err error) error {
	if strings.Contains(stderr, "gh auth login") {
		return fmt.Errorf("%s: %w", name, errAuth)
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "WARNING") {
			return errors.New(clean(strings.TrimPrefix(line, "ERROR: ")))
		}
	}
	return err
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// openURL opens a web page in the browser. Only https: `open` would also launch a file or an app given a file: or
// other URL, and these come from data ghdeck fetched.
func openURL(link string) tea.Cmd {
	return func() tea.Msg {
		u, err := neturl.Parse(link)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return actionMsg{err: fmt.Errorf("not opening %q: only https links open", clean(link))}
		}
		url := u.String()
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		if err := exec.Command(opener, url).Run(); err != nil {
			return actionMsg{err: fmt.Errorf("open %s: %w", url, err)}
		}
		return actionMsg{text: "opened in browser"}
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// pad right-pads to n visible cells, ignoring ANSI styling.
func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func truncate(s string, n int) string {
	if n < 4 {
		n = 4
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	}
}
