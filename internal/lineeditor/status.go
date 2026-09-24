package lineeditor

import (
	"fmt"
	"slices"
)

// SetStatus replaces transient rows above the single-line prompt. Rows must be
// plain printable ASCII (no controls); they are clipped to the viewport. The
// editor retains sole ownership of cursor position, draft, history and writes.
func (t *Terminal) SetStatus(rows []string) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	if slices.Equal(t.status, rows) {
		return nil
	}
	t.status = slices.Clone(rows)
	// ReadLine will render these if there is currently no editable region.
	if t.cursorX == 0 && t.cursorY == 0 && t.selection == nil {
		return nil
	}
	t.repaint(t.statusRows(len(t.line)))
	_, err := t.c.Write(t.outBuf)
	t.outBuf = t.outBuf[:0]
	return err
}

func (t *Terminal) statusRows(lineLength int) int {
	// Leave room for the entire draft and at least one transcript row. If the
	// draft fills the screen, hide status instead of scrolling live frames away.
	inputRows := (visualLength(t.prompt)+lineLength)/t.termWidth + 1
	return min(len(t.status), max(0, t.termHeight-inputRows-1))
}

func (t *Terminal) writeStatus(rows int) {
	t.statusHeight = rows
	for i := 0; i < rows; i++ {
		text := t.status[i]
		if i == rows-1 && rows < len(t.status) {
			text = fmt.Sprintf("+%d more pending (/status)", len(t.status)-i)
		}
		// Keep the last column unused so a row cannot accidentally soft-wrap.
		width := max(0, t.termWidth-1)
		if len(text) > width {
			if width > 3 {
				text = text[:width-3] + "..."
			} else {
				text = text[:width]
			}
		}
		t.queue([]rune(text))
		t.queue([]rune("\r\n"))
		t.cursorY++
	}
}

func (t *Terminal) writePrompt() {
	if t.selection != nil {
		t.writeSelection()
		return
	}
	t.writeStatus(t.statusRows(len(t.line)))
	t.writeLine(t.prompt)
}

func (t *Terminal) clearInput() {
	t.move(t.cursorY, 0, t.cursorX, 0)
	t.cursorX, t.cursorY, t.maxLine = 0, 0, 0
	t.clearLineToRight()
}

func (t *Terminal) repaint(rows int) {
	t.clearInput()
	if t.selection != nil {
		t.writeSelection()
		return
	}
	t.writeStatus(rows)
	t.writeLine(t.prompt)
	if t.echo {
		t.writeLine(t.line)
	}
	t.moveCursorToPos(t.pos)
}

func (t *Terminal) hideStatus() {
	if t.statusHeight != 0 {
		t.repaint(0)
	}
}

func (t *Terminal) fitStatus(lineLength int) {
	if t.selection != nil {
		return
	}
	rows := t.statusRows(lineLength)
	if rows != t.statusHeight {
		t.repaint(rows)
	}
}
