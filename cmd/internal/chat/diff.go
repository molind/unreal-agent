package chat

import (
	"regexp"
	"strconv"
	"strings"
)

// Use standard terminal colors with contrasting foregrounds. No padding or
// erase-line escapes: copying the diff must still yield just its original text.
const (
	diffRemovedStyle = "97;41" // White text, red background.
	diffAddedStyle   = "30;42" // Black text, green background.
)

var diffHunkHeader = regexp.MustCompile(`^@@ -[0-9]+(?:,([0-9]+))? \+[0-9]+(?:,([0-9]+))? @@`)

type diffHighlighter struct{ oldLeft, newLeft int }

func (d *diffHighlighter) lineStyle(line string) string {
	if strings.HasPrefix(line, "@@") {
		d.oldLeft, d.newLeft = 0, 0
		if match := diffHunkHeader.FindStringSubmatch(line); match != nil {
			count := func(value string) int {
				if value == "" {
					return 1
				}
				n, err := strconv.Atoi(value)
				if err != nil {
					return 0
				}
				return n
			}
			d.oldLeft, d.newLeft = count(match[1]), count(match[2])
		}
		return ""
	}
	if strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "Index: ") {
		d.oldLeft, d.newLeft = 0, 0
		return ""
	}
	// Hunk counts distinguish file headers from real changed text such as
	// "--- removed comment" / "+++ added comment" inside a hunk.
	inHunk := d.oldLeft > 0 || d.newLeft > 0
	if !inHunk && (strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "---\t") || strings.HasPrefix(line, "+++\t")) {
		return ""
	}
	switch {
	case strings.HasPrefix(line, "-"):
		d.oldLeft = max(0, d.oldLeft-1)
		return diffRemovedStyle
	case strings.HasPrefix(line, "+"):
		d.newLeft = max(0, d.newLeft-1)
		return diffAddedStyle
	case strings.HasPrefix(line, " "):
		d.oldLeft, d.newLeft = max(0, d.oldLeft-1), max(0, d.newLeft-1)
	}
	return ""
}
