package chat

import (
	"fmt"
	"strings"

	tableast "github.com/yuin/goldmark/extension/ast"
)

// Tables share the safe inline renderer with prose. Size columns in terminal
// cells (not bytes/runes/ANSI), keeping identifiers and URLs intact. If even the
// longest words cannot fit side by side, use labeled records instead of clipping.
func (v markdownView) table(node *tableast.Table, prefix string) string {
	columns := len(node.Alignments)
	if columns == 0 {
		return ""
	}
	var rows [][]string
	widths, desired := make([]int, columns), make([]int, columns)
	for row := node.FirstChild(); row != nil; row = row.NextSibling() {
		cells := make([]string, columns)
		style := ""
		if len(rows) == 0 {
			style = "1"
		}
		i := 0
		for cell := row.FirstChild(); cell != nil && i < columns; cell = cell.NextSibling() {
			cells[i] = v.inline(cell, style)
			for _, line := range strings.Split(cells[i], "\n") {
				words := strings.Fields(line)
				desired[i] = max(desired[i], visibleLength(strings.Join(words, " ")))
				for _, word := range words {
					widths[i] = max(widths[i], visibleLength(word))
				}
			}
			i++
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		return ""
	}
	remaining := v.width - visibleLength(prefix) - (3*columns + 1)
	for i := range widths {
		widths[i] = max(1, widths[i])
		remaining -= widths[i]
	}
	if remaining < 0 {
		return v.tableRecords(rows, prefix)
	}
	// Grow the narrowest column that still needs space, up to its natural
	// width. The budget is at most the viewport width, independent of input.
	for remaining > 0 {
		column := -1
		for i := range widths {
			if widths[i] < desired[i] && (column < 0 || widths[i] < widths[column]) {
				column = i
			}
		}
		if column < 0 {
			break
		}
		widths[column]++
		remaining--
	}

	var out strings.Builder
	rule := func(left, middle, right string) {
		var parts []string
		for _, width := range widths {
			parts = append(parts, strings.Repeat("─", width+2))
		}
		out.WriteString(prefix + paint(v.color, "2", left+strings.Join(parts, middle)+right) + "\n")
	}
	rule("┌", "┬", "┐")
	for r, row := range rows {
		lines, height := make([][]string, columns), 1
		for i, cell := range row {
			lines[i] = tableCellLines(cell, widths[i])
			height = max(height, len(lines[i]))
		}
		for line := 0; line < height; line++ {
			out.WriteString(prefix + paint(v.color, "2", "│"))
			for i := range row {
				value := ""
				if line < len(lines[i]) {
					value = lines[i][line]
				}
				padding := max(0, widths[i]-visibleLength(value))
				left := 0
				switch node.Alignments[i] {
				case tableast.AlignRight:
					left = padding
				case tableast.AlignCenter:
					left = padding / 2
				}
				out.WriteString(" " + strings.Repeat(" ", left) + value + strings.Repeat(" ", padding-left) + " " + paint(v.color, "2", "│"))
			}
			out.WriteByte('\n')
		}
		if r+1 < len(rows) {
			rule("├", "┼", "┤")
		}
	}
	rule("└", "┴", "┘")
	out.WriteByte('\n')
	return out.String()
}

// Balance generated SGR on each physical cell line. A wrapped emphasis/link
// must not color the border or adjacent cell, and must resume on the next line.
func tableCellLines(value string, width int) []string {
	lines := strings.Split(strings.TrimSuffix(wrapProse(value, "", width), "\n"), "\n")
	active := ""
	for i, line := range lines {
		start := active
		for _, style := range terminalStyle.FindAllString(line, -1) {
			if style == "\x1b[0m" {
				active = ""
			} else {
				active += style
			}
		}
		lines[i] = start + line
		if active != "" {
			lines[i] += "\x1b[0m"
		}
	}
	return lines
}

func (v markdownView) tableRecords(rows [][]string, prefix string) string {
	var out strings.Builder
	if len(rows) == 1 {
		for _, header := range rows[0] {
			out.WriteString(wrapProse(header, prefix, v.width))
		}
		return out.String() + "\n"
	}
	for _, row := range rows[1:] {
		for i, cell := range row {
			label := rows[0][i]
			if strings.TrimSpace(terminalStyle.ReplaceAllString(label, "")) == "" {
				label = fmt.Sprintf("Column %d", i+1)
			}
			for _, line := range tableCellLines(label+": "+cell, max(1, v.width-visibleLength(prefix))) {
				out.WriteString(prefix + line + "\n")
			}
		}
		out.WriteByte('\n')
	}
	return out.String()
}
