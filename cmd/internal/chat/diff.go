package chat

import (
	"regexp"
	"strconv"
	"strings"
)

// Fixed xterm palette colors keep both levels readable on light and dark themes:
// pale backgrounds for changed rows, saturated backgrounds for changed text.
// No padding or erase-line escapes: copying still yields just the original text.
const (
	diffRemovedStyle     = "38;5;16;48;5;224" // Black on pale red (#ffd7d7).
	diffAddedStyle       = "38;5;16;48;5;194" // Black on pale green (#d7ffd7).
	diffRemovedTextStyle = "38;5;16;48;5;210" // Black on red (#ff8787).
	diffAddedTextStyle   = "38;5;16;48;5;120" // Black on green (#87ff87).

	// Rendering an arbitrary Markdown diff must not do unbounded quadratic
	// work. Larger replacements retain common edges and highlight the middle.
	diffMaxCells = 1 << 20
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

// highlightDiff decorates sanitized rows in place. Compare complete change
// blocks, not rows by index: inserting a row must not misalign every later row.
func highlightDiff(lines []string) {
	var d diffHighlighter
	styles := make([]string, len(lines))
	for i, line := range lines {
		styles[i] = d.lineStyle(line)
	}
	for start := 0; start < len(lines); {
		if styles[start] == "" {
			start++
			continue
		}
		end, added := start, false
		for end < len(lines) {
			style := styles[end]
			if style == "" && lines[end] != `\ No newline at end of file` {
				break
			}
			if added && style == diffRemovedStyle {
				break // Another replacement in a headerless diff fragment.
			}
			added = added || style == diffAddedStyle
			end++
		}
		highlightDiffBlock(lines[start:end], styles[start:end])
		start = end
	}
}

func highlightDiffBlock(lines, styles []string) {
	var before, after []rune
	for i, line := range lines {
		switch styles[i] {
		case diffRemovedStyle:
			before = append(append(before, []rune(line[1:])...), '\n')
		case diffAddedStyle:
			after = append(append(after, []rune(line[1:])...), '\n')
		}
	}
	removed, added := diffChangedRunes(before, after)
	oldPos, newPos := 0, 0
	for i, line := range lines {
		switch styles[i] {
		case diffRemovedStyle:
			n := len([]rune(line[1:]))
			lines[i] = paintDiffLine(line, removed[oldPos:oldPos+n], diffRemovedStyle, diffRemovedTextStyle)
			oldPos += n + 1
		case diffAddedStyle:
			n := len([]rune(line[1:]))
			lines[i] = paintDiffLine(line, added[newPos:newPos+n], diffAddedStyle, diffAddedTextStyle)
			newPos += n + 1
		}
	}
}

// diffChangedRunes finds a bounded longest common subsequence. Working in runes
// preserves UTF-8; retaining all common runs highlights separate edits without
// also emphasizing the unchanged text between them. Newlines participate in the
// comparison but the diff signs and no-newline annotations do not.
func diffChangedRunes(before, after []rune) (removed, added []bool) {
	removed, added = make([]bool, len(before)), make([]bool, len(after))
	start, oldEnd, newEnd := 0, len(before), len(after)
	for start < oldEnd && start < newEnd && before[start] == after[start] {
		start++
	}
	for oldEnd > start && newEnd > start && before[oldEnd-1] == after[newEnd-1] {
		oldEnd--
		newEnd--
	}
	for i := start; i < oldEnd; i++ {
		removed[i] = true
	}
	for i := start; i < newEnd; i++ {
		added[i] = true
	}
	a, b := before[start:oldEnd], after[start:newEnd]
	if len(a) == 0 || len(b) == 0 || len(a)+1 > diffMaxCells/(len(b)+1) {
		return removed, added
	}
	width := len(b) + 1
	lcs := make([]uint32, (len(a)+1)*width)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i*width+j] = 1 + lcs[(i+1)*width+j+1]
			} else {
				lcs[i*width+j] = max(lcs[(i+1)*width+j], lcs[i*width+j+1])
			}
		}
	}
	for i, j := 0, 0; i < len(a) && j < len(b); {
		if a[i] == b[j] {
			removed[start+i], added[start+j] = false, false
			i++
			j++
		} else if lcs[(i+1)*width+j] >= lcs[i*width+j+1] {
			i++
		} else {
			j++
		}
	}
	return removed, added
}

func paintDiffLine(line string, changed []bool, base, strong string) string {
	var out strings.Builder
	out.WriteString("\x1b[" + base + "m")
	out.WriteByte(line[0]) // The +/- marker always keeps the pale row background.
	active := false
	for i, r := range []rune(line[1:]) {
		if changed[i] != active {
			style := base
			if changed[i] {
				style = strong
			}
			out.WriteString("\x1b[" + style + "m")
			active = changed[i]
		}
		out.WriteRune(r)
	}
	// Reset before the newline so following rows and the prompt stay neutral.
	out.WriteString("\x1b[0m")
	return out.String()
}
