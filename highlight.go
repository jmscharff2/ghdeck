package main

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/lipgloss"
)

// Syntax colours for the diff. Added and removed lines keep their meaning in a background tint, so the foreground is
// free for the code's own colours; a file chroma does not know keeps plain green and red text.

var (
	cAddBg = lipgloss.AdaptiveColor{Light: "#E6F4EA", Dark: "#16301F"}
	cDelBg = lipgloss.AdaptiveColor{Light: "#FDECEC", Dark: "#3A1A1D"}

	synKeyword = lipgloss.AdaptiveColor{Light: "#8250DF", Dark: "#C4A7FF"}
	synString  = lipgloss.AdaptiveColor{Light: "#0A3069", Dark: "#A5D6FF"}
	synNumber  = lipgloss.AdaptiveColor{Light: "#0550AE", Dark: "#79C0FF"}
	synComment = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#8B949E"}
	synFunc    = lipgloss.AdaptiveColor{Light: "#6639BA", Dark: "#D2A8FF"}
	synType    = lipgloss.AdaptiveColor{Light: "#953800", Dark: "#FFA657"}
)

// styledRun is a stretch of a line in one style.
type styledRun struct {
	text  string
	style lipgloss.Style
}

// highlighter tokenises a file's lines. Each line is read alone, so a string or comment spanning lines is coloured
// as best one line allows; a diff has only fragments of a file anyway.
type highlighter struct {
	lexer chroma.Lexer
	cache map[string][]styledRun
}

func newHighlighter(path string) *highlighter {
	l := lexers.Match(path)
	if l == nil {
		return &highlighter{}
	}
	return &highlighter{lexer: chroma.Coalesce(l), cache: map[string][]styledRun{}}
}

// runs splits a line into styled stretches, or one plain stretch when the file's language is unknown.
func (h *highlighter) runs(text string) []styledRun {
	if h == nil || h.lexer == nil {
		return []styledRun{{text: text, style: lipgloss.NewStyle()}}
	}
	if r, ok := h.cache[text]; ok {
		return r
	}
	it, err := h.lexer.Tokenise(nil, text)
	if err != nil {
		return []styledRun{{text: text, style: lipgloss.NewStyle()}}
	}
	var out []styledRun
	for _, t := range it.Tokens() {
		v := strings.TrimRight(t.Value, "\n")
		if v == "" {
			continue
		}
		out = append(out, styledRun{text: v, style: tokenStyle(t.Type)})
	}
	if len(h.cache) < 20000 {
		h.cache[text] = out
	}
	return out
}

func tokenStyle(t chroma.TokenType) lipgloss.Style {
	s := lipgloss.NewStyle()
	switch {
	case t.InCategory(chroma.Comment):
		return s.Foreground(synComment).Italic(true)
	case t.InCategory(chroma.Keyword):
		return s.Foreground(synKeyword)
	case t.InCategory(chroma.LiteralString):
		return s.Foreground(synString)
	case t.InCategory(chroma.LiteralNumber):
		return s.Foreground(synNumber)
	case t == chroma.NameFunction || t == chroma.NameBuiltin:
		return s.Foreground(synFunc)
	case t == chroma.NameClass || t == chroma.KeywordType || t == chroma.NameTag:
		return s.Foreground(synType)
	}
	return s
}

// slice draws runes [from, to) of a line's runs, each run in its style over bg (none when bg is nil).
func slice(runs []styledRun, from, to int, bg lipgloss.TerminalColor, fg lipgloss.TerminalColor) string {
	var b strings.Builder
	pos := 0
	for _, r := range runs {
		rs := []rune(r.text)
		start, end := max(from, pos), min(to, pos+len(rs))
		if start < end {
			st := r.style
			if fg != nil {
				st = st.Foreground(fg)
			}
			if bg != nil {
				st = st.Background(bg)
			}
			b.WriteString(st.Render(string(rs[start-pos : end-pos])))
		}
		pos += len(rs)
		if pos >= to {
			break
		}
	}
	return b.String()
}
