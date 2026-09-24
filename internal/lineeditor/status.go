package lineeditor

import (
	"fmt"
	"slices"
	"strings"
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

// SetPromptInfo adds a quiet, single-cell context/rule row immediately above
// the prompt. It shares the status viewport and disappears before submission.
// Controls and ambiguous-width Unicode in text are escaped, as in selections.
// Color affects only editor-owned decorations, never the draft itself.
func (t *Terminal) SetPromptInfo(text string, color bool) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.promptInfo == text && t.color == color {
		return nil
	}
	t.promptInfo, t.color = text, color
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
	rows := len(t.status)
	if t.promptInfo != "" {
		rows++
	}
	return min(rows, max(0, t.termHeight-inputRows-1))
}

func (t *Terminal) writeStatus(rows int) {
	t.statusHeight = rows
	info := t.selection == nil && t.promptInfo != "" && rows > 0
	if info {
		rows--
	}
	if t.color {
		t.queue([]rune("\x1b[2m"))
	}
	for i := 0; i < rows; i++ {
		text := t.status[i]
		if i == rows-1 && rows < len(t.status) {
			text = fmt.Sprintf("+%d more pending (/status)", len(t.status)-i)
		}
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
	if info {
		width := max(0, t.termWidth-1)
		row := []rune("── ")
		row = append(row, selectionText(t.promptInfo, max(0, width-len(row)-1))...)
		if len(row) < width {
			row = append(row, []rune(" "+strings.Repeat("─", width-len(row)-1))...)
		}
		t.queue(row[:min(len(row), width)])
		t.queue([]rune("\r\n"))
		t.cursorY++
	}
	if t.color {
		t.queue([]rune("\x1b[0m"))
	}
}

func (t *Terminal) writeInputPrompt() {
	if t.color {
		t.queue([]rune("\x1b[1;36m"))
	}
	t.writeLine(t.prompt)
	if t.color {
		t.queue([]rune("\x1b[0m"))
	}
}

func (t *Terminal) writePrompt() {
	if t.selection != nil {
		t.writeSelection()
		return
	}
	t.writeStatus(t.statusRows(len(t.line)))
	t.writeInputPrompt()
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
	t.writeInputPrompt()
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
