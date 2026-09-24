package main

import (
	"strconv"
	"strings"
	"testing"
)

// A deliberately small VT100 screen oracle for the controls emitted by x/term.
// Unlike searching output bytes, this checks what remains visible and where
// the cursor actually sits after wrapped asynchronous redraws.
func assertDraftOnScreen(t *testing.T, output, draft string, width, fromEnd int) {
	t.Helper()
	rows := [][]rune{make([]rune, width)}
	x, y := 0, 0
	row := func() {
		for y >= len(rows) {
			rows = append(rows, make([]rune, width))
		}
	}
	rs := []rune(output)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == 27 && i+1 < len(rs) && rs[i+1] == 91 {
			j := i + 2
			for j < len(rs) && !(rs[j] >= 64 && rs[j] <= 126) {
				j++
			}
			if j == len(rs) {
				break
			}
			count, _ := strconv.Atoi(string(rs[i+2 : j]))
			if count == 0 {
				count = 1
			}
			switch rs[j] {
			case 65:
				y = max(0, y-count)
			case 66:
				y += count
				row()
			case 67:
				x = min(width-1, x+count)
			case 68:
				x = max(0, x-count)
			case 74:
				row()
				for k := min(x, width-1); k < width; k++ {
					rows[y][k] = 32
				}
				for yy := y + 1; yy < len(rows); yy++ {
					for k := range rows[yy] {
						rows[yy][k] = 32
					}
				}
			case 75:
				row()
				for k := min(x, width-1); k < width; k++ {
					rows[y][k] = 32
				}
			}
			i = j
			continue
		}
		switch r {
		case 13:
			x = 0
		case 10:
			y++
			row()
		case 8:
			x = max(0, x-1)
		default:
			if r < 32 {
				continue
			}
			if x == width {
				x = 0
				y++
				row()
			}
			rows[y][x] = r
			x++
		}
	}
	var visible strings.Builder
	for _, row := range rows {
		for _, r := range row {
			if r == 0 {
				r = 32
			}
			visible.WriteRune(r)
		}
	}
	cells := []rune(visible.String())
	text := string(cells)
	index := strings.LastIndex(text, "you> "+draft)
	if index < 0 {
		t.Fatalf("draft not visible after redraw; want %q; screen: %q", draft, text)
	}
	prefix := len([]rune(text[:index])) + len([]rune("you> "+draft))
	if y*width+x != prefix-fromEnd {
		t.Fatalf("screen cursor=%d, want %d; screen: %q", y*width+x, prefix-fromEnd, text)
	}
	// Old longer prompts/drafts must not leave stale lines below the new draft.
	if strings.TrimSpace(string(cells[prefix:])) != "" {
		t.Fatalf("stale content below draft: %q", string(cells[prefix:]))
	}
}
