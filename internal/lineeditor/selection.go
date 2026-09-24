package lineeditor

import (
	"fmt"
	"slices"
	"strconv"
	"unicode"
)

// Choice carries an opaque identity, a one-line label and selected-item detail.
// Labels use single-cell text: ASCII and Cyrillic letters remain readable;
// controls and other Unicode are escaped, never executed by a terminal.
type Choice struct{ Value, Label, Detail string }

// Selection is returned by ReadLine instead of a submitted line. Canceled
// means Escape dismissed the chooser. It is never added to editing history.
type Selection struct {
	// Context identifies a scoped asynchronous chooser, including Escape results.
	Context  string
	Value    string
	Canceled bool
}

func (*Selection) Error() string { return "terminal selection" }

type selection struct {
	id         string
	title      string
	choices    []Choice
	index, top int
}

// OpenSelection uses the editor input reader, lock and transient cursor origin.
// It preserves the draft and live status; it does not block asynchronous Write.
// The caller must finish handling the previous line before starting ReadLine.
func (t *Terminal) OpenSelection(title string, choices []Choice) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	if len(choices) == 0 {
		return nil
	}
	t.selection = &selection{title: title, choices: slices.Clone(choices)}
	t.repaint(0)
	_, err := t.c.Write(t.outBuf)
	t.outBuf = t.outBuf[:0]
	return err
}

func selectionText(text string, width int) []rune {
	var out []rune
	for _, r := range text {
		if r >= 32 && r <= 126 || unicode.IsLetter(r) && unicode.In(r, unicode.Cyrillic) {
			out = append(out, r)
		} else {
			quoted := strconv.QuoteRuneToASCII(r)
			out = append(out, []rune(quoted[1:len(quoted)-1])...)
		}
		if len(out) > width {
			break
		}
	}
	if len(out) > width {
		out = out[:width]
		if width >= 3 {
			copy(out[width-3:], []rune("..."))
		}
	}
	return out
}

func (t *Terminal) selectionRow(text string, newline bool) {
	row := selectionText(text, max(0, t.termWidth-1))
	t.queue(row)
	t.cursorX = len(row)
	if newline {
		t.queue([]rune("\r\n"))
		t.cursorY++
		t.cursorX = 0
	}
}

func (t *Terminal) writeSelection() {
	s := t.selection
	// One transcript row remains available. Small windows prioritize selection
	// over decorations and status, but never hide the selected item.
	available := max(1, t.termHeight-1)
	live := min(len(t.status), 2, max(0, available-4))
	t.writeStatus(live)
	available -= live
	decorated := available >= 3
	rows := available
	if decorated {
		t.selectionRow(fmt.Sprintf("%s %d/%d: Up/Down Enter Esc", s.title, s.index+1, len(s.choices)), true)
		rows -= 2
	}
	rows = min(rows, len(s.choices))
	s.top = min(s.top, max(0, len(s.choices)-rows))
	if s.index < s.top {
		s.top = s.index
	}
	if s.index >= s.top+rows {
		s.top = s.index - rows + 1
	}
	cursorRow := 0
	for i := s.top; i < s.top+rows; i++ {
		marker := " "
		if i == s.index {
			marker = ">"
			cursorRow = t.cursorY
		}
		t.selectionRow(fmt.Sprintf("%s %d %s", marker, i+1, s.choices[i].Label), decorated || i < s.top+rows-1)
	}
	if decorated {
		t.selectionRow(s.choices[s.index].Detail, false)
	}
	// The hardware cursor also follows the visible marker. Column zero avoids
	// wrapping the final row even on a one-column terminal.
	t.move(t.cursorY-cursorRow, 0, t.cursorX, 0)
	t.cursorY = cursorRow
	t.cursorX = 0
}

func (t *Terminal) selectionKey(key rune) *Selection {
	s := t.selection
	switch key {
	case keyUp, keyHistoryPrev:
		s.index = max(0, s.index-1)
	case keyDown, keyHistoryNext:
		s.index = min(len(s.choices)-1, s.index+1)
	case keyEnter, keyLF, keyEscape:
		result := &Selection{Context: s.id, Canceled: key == keyEscape}
		if key != keyEscape {
			result.Value = s.choices[s.index].Value
		}
		t.selection = nil
		t.repaint(t.statusRows(len(t.line)))
		return result
	}
	t.repaint(0)
	return nil
}

// CloseSelection erases the chooser on shutdown (including EOF/error).
func (t *Terminal) CloseSelection() error {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.selection == nil {
		return nil
	}
	t.selection = nil
	t.repaint(t.statusRows(len(t.line)))
	_, err := t.c.Write(t.outBuf)
	t.outBuf = t.outBuf[:0]
	return err
}

// TryOpenSelection may be called while ReadLine waits for input. It never
// replaces another chooser. Results retain id even on Escape, so a delayed
// event cannot be mistaken for a different question or a session selection.
func (t *Terminal) TryOpenSelection(id, title string, choices []Choice) (bool, error) {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.selection != nil || len(choices) == 0 {
		return false, nil
	}
	t.selection = &selection{id: id, title: title, choices: slices.Clone(choices)}
	t.repaint(0)
	_, err := t.c.Write(t.outBuf)
	t.outBuf = t.outBuf[:0]
	return true, err
}

// CloseSelectionID dismisses only the matching scoped question. In particular,
// clearing an obsolete approval must not close an unrelated resume chooser.
func (t *Terminal) CloseSelectionID(id string) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.selection == nil || t.selection.id != id {
		return nil
	}
	t.selection = nil
	t.repaint(t.statusRows(len(t.line)))
	_, err := t.c.Write(t.outBuf)
	t.outBuf = t.outBuf[:0]
	return err
}
