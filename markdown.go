package main

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Descriptions and comments are GitHub Markdown. ghdeck draws enough of it to read comfortably in a terminal:
// headings, lists, task boxes, quotes, code blocks, emphasis and links, with the HTML comments of pull request
// templates and common HTML tags left out. It runs on text clean has already been through.

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTag     = regexp.MustCompile(`(?i)</?(?:br|p|div|details|summary|img|sub|sup|kbd|b|i|em|strong|a|picture|source|span|hr|table|tr|td|th|thead|tbody|ul|ol|li|h[1-6]|code|pre|center|video)\b[^>]*>`)
	mdImage     = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink      = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)[^)]*\)`)
	mdBold      = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	mdCode      = regexp.MustCompile("`([^`]+)`")
	mdHeading   = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	mdBullet    = regexp.MustCompile(`^(\s*)(?:[-*+]|\d+[.)])\s+(.*)$`)
	mdTask      = regexp.MustCompile(`^\[([ xX])\]\s+(.*)$`)
)

var sCode = lipgloss.NewStyle().Foreground(cAccent)

// renderMarkdown draws body at width w, one string per screen row.
func renderMarkdown(body string, w int) []string {
	w = max(20, w)
	body = htmlComment.ReplaceAllString(body, "")
	var out []string
	inCode, suggestion := false, false
	blank := true
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inCode = !inCode
			// A suggested change: the lines that would replace the ones commented on.
			suggestion = inCode && strings.TrimSpace(trimmed[3:]) == "suggestion"
			if suggestion {
				out = append(out, sWarn.Render("suggested change:"))
			}
			continue
		}
		if inCode {
			text := truncate(strings.ReplaceAll(line, "\t", "    "), w-2)
			if suggestion {
				out = append(out, sGood.Render("+ "+text))
			} else {
				out = append(out, sMuted.Render("│ ")+text)
			}
			blank = false
			continue
		}
		line = htmlTag.ReplaceAllString(line, "")
		trimmed = strings.TrimSpace(line)
		if trimmed == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		if trimmed == "---" || trimmed == "***" || trimmed == "___" {
			out = append(out, sRule.Render(strings.Repeat("─", min(w, 40))))
			continue
		}
		if m := mdHeading.FindStringSubmatch(trimmed); m != nil {
			style := sBold
			if len(m[1]) <= 2 {
				style = sTitle
			}
			out = append(out, wrapStyledText(style.Render(inline(m[2])), w, "")...)
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			text := strings.TrimSpace(strings.TrimLeft(trimmed, "> "))
			for _, l := range wrapText(inline(text), w-2) {
				out = append(out, sMuted.Render("▎ ")+sMuted.Render(l))
			}
			continue
		}
		if m := mdBullet.FindStringSubmatch(line); m != nil {
			indent := strings.Repeat(" ", min(len(m[1]), 8))
			mark, text := "• ", m[2]
			if t := mdTask.FindStringSubmatch(text); t != nil {
				mark, text = "☐ ", t[2]
				if t[1] != " " {
					mark = sGood.Render("☑ ")
				}
			}
			out = append(out, wrapStyledText(indent+mark+inline(text), w, indent+"  ")...)
			continue
		}
		out = append(out, wrapStyledText(inline(trimmed), w, "")...)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// inline styles emphasis, code and links within a line.
func inline(s string) string {
	s = mdImage.ReplaceAllString(s, "[image: $1]")
	s = mdLink.ReplaceAllStringFunc(s, func(l string) string {
		m := mdLink.FindStringSubmatch(l)
		return lipgloss.NewStyle().Underline(true).Render(m[1])
	})
	s = mdCode.ReplaceAllStringFunc(s, func(c string) string { return sCode.Render(strings.Trim(c, "`")) })
	s = mdBold.ReplaceAllStringFunc(s, func(b string) string { return sBold.Render(strings.Trim(b, "*_")) })
	return s
}

// wrapStyledText wraps a styled line at w, indenting the rows after the first.
func wrapStyledText(s string, w int, hang string) []string {
	rows := strings.Split(lipgloss.NewStyle().Width(max(10, w-lipgloss.Width(hang))).Render(s), "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " ")
		if i > 0 {
			rows[i] = hang + strings.TrimLeft(rows[i], " ")
		}
	}
	return rows
}

// wrapText wraps plain text at w, keeping its line breaks.
func wrapText(s string, w int) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			continue
		}
		out = append(out, wrapStyledText(line, w, "")...)
	}
	return out
}
