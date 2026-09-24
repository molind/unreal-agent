package lineeditor

import "slices"

// KeyNewline is a callback key for inserting LF without submitting ReadLine.
// Applications can validate byte/cell limits in AutoCompleteCallback first.
const KeyNewline = 0xdb00

type draftPoint struct{ x, y int }
type draftLayout struct {
	rows [][]rune
	pos  []draftPoint
}

func (t *Terminal) multiline() bool { return t.echo && slices.Contains(t.line, '\n') }

// Multiline drafts reserve the last column rather than relying on a terminal's
// pending autowrap state. Positions are source-rune positions, never byte offsets.
func (t *Terminal) layoutDraft() draftLayout {
	l := draftLayout{rows: [][]rune{nil}, pos: make([]draftPoint, len(t.line)+1)}
	width := max(1, t.termWidth-1)
	put := func(r rune) {
		last := len(l.rows) - 1
		if r == '\n' {
			l.rows = append(l.rows, nil)
			return
		}
		if len(l.rows[last]) == width {
			l.rows = append(l.rows, nil)
			last++
		}
		l.rows[last] = append(l.rows[last], r)
	}
	point := func() draftPoint { return draftPoint{len(l.rows[len(l.rows)-1]), len(l.rows) - 1} }
	for _, r := range t.prompt {
		put(r)
	}
	l.pos[0] = point()
	for i, r := range t.line {
		put(r)
		l.pos[i+1] = point()
	}
	return l
}

func (t *Terminal) writeMultiline() {
	l := t.layoutDraft()
	height := max(1, t.termHeight-t.statusHeight-1)
	cursor := l.pos[t.pos]
	t.draftTop = min(t.draftTop, max(0, len(l.rows)-height))
	if cursor.y < t.draftTop {
		t.draftTop = cursor.y
	}
	if cursor.y >= t.draftTop+height {
		t.draftTop = cursor.y - height + 1
	}
	end := min(len(l.rows), t.draftTop+height)
	for i := t.draftTop; i < end; i++ {
		row := l.rows[i]
		// The plain prompt stays part of the same layout, not another renderer.
		if i == 0 && t.color && len(t.prompt) <= len(row) {
			t.queue([]rune("\x1b[1;36m"))
			t.queue(row[:len(t.prompt)])
			t.queue([]rune("\x1b[0m"))
			t.queue(row[len(t.prompt):])
		} else {
			t.queue(row)
		}
		t.cursorX = len(row)
		if i+1 < end {
			t.queue([]rune("\r\n"))
			t.cursorX = 0
			t.cursorY++
		}
	}
	t.maxLine = t.cursorY
	t.moveCursor(cursor.x, t.statusHeight+cursor.y-t.draftTop)
}

func (t *Terminal) moveMultiline(pos int) {
	l := t.layoutDraft()
	p := l.pos[pos]
	height := max(1, t.termHeight-t.statusHeight-1)
	if p.y < t.draftTop || p.y >= t.draftTop+height {
		t.pos = pos
		t.repaint(t.statusRows(len(t.line)))
		return
	}
	t.moveCursor(p.x, t.statusHeight+p.y-t.draftTop)
}

func (t *Terminal) moveVertical(delta int) {
	l := t.layoutDraft()
	p := l.pos[t.pos]
	row := p.y + delta
	if row < 0 || row >= len(l.rows) {
		return
	}
	// Preserve the text column, not the first row's decorative prompt width.
	// Continuation lines themselves start at column zero.
	column := p.x
	first := l.pos[0]
	if p.y == first.y {
		column -= first.x
	}
	if row == first.y {
		column += first.x
	}
	best, distance := t.pos, int(^uint(0)>>1)
	for i, candidate := range l.pos {
		if candidate.y != row {
			continue
		}
		d := candidate.x - column
		if d < 0 {
			d = -d
		}
		if d < distance {
			best, distance = i, d
		}
	}
	t.pos = best
	t.moveCursorToPos(t.pos)
}
