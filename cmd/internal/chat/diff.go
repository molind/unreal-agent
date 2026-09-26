package chat

import (
	"regexp"
	"strconv"
	"strings"
)

// Dark-theme xterm palette: subdued rows, brighter changed tokens in bold white.
// Reset in the base style clears bold when leaving an emphasized span. No
// padding or erase-line escapes: copying still yields just the original text.
const (
	diffRemovedStyle     = "0;38;5;224;48;5;52"  // Pale red on dark red (#5f0000).
	diffAddedStyle       = "0;38;5;194;48;5;22"  // Pale green on dark green (#005f00).
	diffRemovedTextStyle = "1;38;5;231;48;5;124" // Bold white on red (#af0000).
	diffAddedTextStyle   = "1;38;5;231;48;5;34"  // Bold white on green (#00af00).

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
	var before, after []diffRow
	for i, line := range lines {
		switch styles[i] {
		case diffRemovedStyle:
			before = append(before, newDiffRow(i, line[1:]))
		case diffAddedStyle:
			after = append(after, newDiffRow(i, line[1:]))
		}
	}
	budget := diffMaxCells
	changes := make(map[int][]bool)
	for _, pair := range diffRowPairs(before, after, &budget) {
		a, b := before[pair[0]], after[pair[1]]
		removed, added := diffChangedTokens(a.tokens, b.tokens, &budget)
		changes[a.index], changes[b.index] = removed, added
	}
	for _, row := range before {
		lines[row.index] = paintDiffLine(lines[row.index], row.tokens, changes[row.index], diffRemovedStyle, diffRemovedTextStyle)
	}
	for _, row := range after {
		lines[row.index] = paintDiffLine(lines[row.index], row.tokens, changes[row.index], diffAddedStyle, diffAddedTextStyle)
	}
}

// Compare whole tokens only within paired rows. Common letters in otherwise
// different identifiers must not produce a checkerboard of unchanged fragments.
// The quadratic budget is shared with row matching for the entire change block.
func diffChangedTokens(before, after []string, budget *int) (removed, added []bool) {
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
	if len(a) == 0 || len(b) == 0 || len(a)+1 > *budget/(len(b)+1) {
		return removed, added
	}
	width := len(b) + 1
	cells := (len(a) + 1) * width
	*budget -= cells
	lcs := make([]uint32, cells)
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

func paintDiffLine(line string, tokens []string, changed []bool, base, strong string) string {
	// An unpaired (wholly added/deleted) row has one uniform background,
	// including the sign. Do not invent intraline matches with another row.
	if changed == nil {
		return paint(true, base, line)
	}
	var out strings.Builder
	out.WriteString("\x1b[" + base + "m")
	out.WriteByte(line[0])
	active := false
	for i, token := range tokens {
		if changed[i] != active {
			style := base
			if changed[i] {
				style = strong
			}
			out.WriteString("\x1b[" + style + "m")
			active = changed[i]
		}
		out.WriteString(token)
	}
	// Reset before the newline so following rows and the prompt stay neutral.
	out.WriteString("\x1b[0m")
	return out.String()
}
