package lineeditor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

// ViewerLimit bounds both a report snapshot and queued asynchronous transcript
// output. On queue overflow the viewer closes and delivers all output normally.
const ViewerLimit = 1 << 20

var ErrViewerTooLarge = errors.New("report exceeds the 1 MiB viewer limit")

type ViewerClosed struct{ Overflow bool }

func (*ViewerClosed) Error() string { return "terminal viewer closed" }

type viewerRow struct{ start, end int }
type viewer struct {
	title   string
	text    []rune
	rows    []viewerRow
	top     int
	pending bytes.Buffer
}

// OpenViewer uses an alternate screen, not transcript scrollback. It is a
// read-only snapshot under the editor's existing lock/reader. Draft, cursor,
// history and live status are retained; a selector and viewer never overlap.
func (t *Terminal) OpenViewer(title, text string) (bool, error) {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.selection != nil || t.viewer != nil {
		return false, nil
	}
	if len(text) > ViewerLimit || len(title) > 1024 {
		return false, ErrViewerTooLarge
	}
	v := &viewer{title: title, text: viewerText(text)}
	// Clear only the editable tail on the primary screen, then save that blank
	// origin. Resize in the viewer cannot leave old draft/status rows behind.
	t.clearInput()
	t.statusHeight = 0
	t.viewer = v
	v.layout(max(1, t.termWidth-1))
	t.queue([]rune("\x1b[?1049h"))
	t.drawViewer()
	return true, t.flushViewer()
}

// CloseViewer restores the primary screen and delivers queued output. Call on
// shutdown, EOF, output failure, or an externally delivered interrupt as well.
// ReadLine reports a ViewerClosed event on its next handoff, never a user line.
func (t *Terminal) CloseViewer() (bool, error) {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.viewer == nil {
		return false, nil
	}
	return true, t.closeViewer(false)
}

func (t *Terminal) flushViewer() error {
	buf := t.outBuf
	t.outBuf = t.outBuf[:0]
	n, err := t.c.Write(buf)
	if err == nil && n != len(buf) {
		err = io.ErrShortWrite
	}
	return err
}

func (t *Terminal) closeViewer(overflow bool) error {
	v := t.viewer
	if v == nil {
		return nil
	}
	if t.color {
		t.queue([]rune("\x1b[0m"))
	}
	t.queue([]rune("\x1b[?1049l"))
	// Retain the viewer on failure so shutdown can retry leaving alternate mode.
	if err := t.flushViewer(); err != nil {
		return err
	}
	t.viewer = nil
	t.viewerClosed = &ViewerClosed{Overflow: overflow}
	t.cursorX, t.cursorY, t.maxLine, t.statusHeight = 0, 0, 0, 0
	if v.pending.Len() > 0 {
		// The primary editable origin was cleared before entering the viewer.
		// Deliver complete original writes before repainting the latest draft.
		if n, err := writeWithCRLF(t.c, v.pending.Bytes()); err != nil {
			return err
		} else if n != v.pending.Len() {
			return io.ErrShortWrite
		}
	}
	t.repaint(t.statusRows(len(t.line)))
	return t.flushViewer()
}

// viewerText exposes only predictable single-cell characters. Unlike selection
// labels it never clips content: long rows wrap, and ambiguous-width characters
// and terminal controls are escaped rather than executed or silently dropped.
func viewerText(text string) []rune {
	var out []rune
	for _, r := range text {
		switch {
		case r == '\n':
			out = append(out, r)
		case r == '\t':
			out = append(out, ' ', ' ', ' ', ' ')
		case r >= 32 && r <= 126,
			unicode.IsLetter(r) && unicode.In(r, unicode.Latin, unicode.Greek, unicode.Cyrillic),
			strings.ContainsRune("—–…·─│→←↑↓", r):
			out = append(out, r)
		default:
			quoted := strconv.QuoteRuneToASCII(r)
			out = append(out, []rune(quoted[1:len(quoted)-1])...)
		}
	}
	return out
}

func (v *viewer) layout(width int) {
	anchor := 0
	if len(v.rows) > 0 {
		anchor = v.rows[min(v.top, len(v.rows)-1)].start
	}
	v.rows = v.rows[:0]
	start := 0
	for i, r := range v.text {
		if r == '\n' {
			v.rows = append(v.rows, viewerRow{start, i})
			start = i + 1
		} else if i-start == width {
			v.rows = append(v.rows, viewerRow{start, i})
			start = i
		}
	}
	if start < len(v.text) || len(v.rows) == 0 {
		v.rows = append(v.rows, viewerRow{start, len(v.text)})
	}
	v.top = 0
	for i, row := range v.rows {
		if row.start <= anchor {
			v.top = i
		} else {
			break
		}
	}
}
func (t *Terminal) viewerBody() (top, rows int) {
	if t.termHeight >= 3 {
		top = 1
	}
	footer := 0
	if t.termHeight >= 2 {
		footer = 1
	}
	return top, max(1, t.termHeight-top-footer)
}
func (t *Terminal) viewerLine(row int, text []rune, dim bool) {
	t.queue([]rune(fmt.Sprintf("\x1b[%d;1H", row+1)))
	if dim && t.color {
		t.queue([]rune("\x1b[2m"))
	}
	t.queue(text[:min(len(text), max(1, t.termWidth-1))])
	if dim && t.color {
		t.queue([]rune("\x1b[0m"))
	}
}
func (t *Terminal) drawViewer() {
	v := t.viewer
	if v == nil {
		return
	}
	top, rows := t.viewerBody()
	v.top = max(0, min(v.top, len(v.rows)-rows))
	if t.color {
		t.queue([]rune("\x1b[0m"))
	}
	t.queue([]rune("\x1b[H\x1b[2J"))
	if top != 0 {
		title := strings.ReplaceAll(v.title, "\n", " ") + " (snapshot)"
		t.viewerLine(0, viewerText(title), true)
	}
	for i := 0; i < rows && v.top+i < len(v.rows); i++ {
		row := v.rows[v.top+i]
		t.viewerLine(top+i, v.text[row.start:row.end], false)
	}
	if t.termHeight >= 2 {
		footer := fmt.Sprintf("Esc back | %d-%d/%d | Up/Down PgUp/PgDn Home/End", v.top+1, min(v.top+rows, len(v.rows)), len(v.rows))
		if v.pending.Len() > 0 {
			footer = fmt.Sprintf("Esc back | %d new bytes | %d-%d/%d", v.pending.Len(), v.top+1, min(v.top+rows, len(v.rows)), len(v.rows))
		}
		t.viewerLine(t.termHeight-1, []rune(footer), true)
	}
	// Absolute placement cancels delayed wrapping even on a one-column terminal.
	t.queue([]rune(fmt.Sprintf("\x1b[%d;1H", t.termHeight)))
}
func (t *Terminal) viewerKey(key rune) error {
	v := t.viewer
	_, rows := t.viewerBody()
	switch key {
	case keyEscape, keyCtrlC, 'q':
		return t.closeViewer(false)
	case keyUp, keyHistoryPrev, 'k':
		v.top--
	case keyDown, keyHistoryNext, 'j', keyEnter, KeyNewline:
		v.top++
	case keyPageUp, 'b':
		v.top -= max(1, rows-1)
	case keyPageDown, ' ':
		v.top += max(1, rows-1)
	case keyHome, 'g':
		v.top = 0
	case keyEnd, 'G':
		v.top = len(v.rows) - rows
	default:
		return nil // Never edit the draft or invoke paste/autocomplete here.
	}
	t.drawViewer()
	return nil
}
