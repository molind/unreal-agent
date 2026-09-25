package chat

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"

	"github.com/unreallabsai/unreal-agent/internal/lineeditor"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

var errInputInterrupt = errors.New("terminal input interrupted")

// x/term owns editing, history, wrapping, and the lock that serializes redraws
// with ReadLine. The application remains the sole consumer of harness events.
type terminalUI struct {
	editor         *lineeditor.Terminal
	input          *os.File
	output         io.Writer
	state          *term.State
	control        int
	width          int  // Owned by the application event loop, like display state.
	viewerDisabled bool // Unknown/non-xterm TERM: never risk clearing primary history.
	resize         chan os.Signal
	failures       chan error
	once           sync.Once
	// Transport and immutable paste attachments belong to the input goroutine.
	reader                    *bufio.Reader
	paste                     bool
	sequence, pending, pasted []byte
	attachments               map[rune]string
	nextAttachment            rune
	history                   lineeditor.History
	rejected                  string
}

func openTerminal(input io.ReadCloser, output io.Writer, getenv func(string) string) (*terminalUI, error) {
	in, ok := input.(*os.File)
	out, outOK := output.(*os.File)
	if !ok || !outOK || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) || getenv("TERM") == "dumb" || getenv("TERM") == "" {
		return nil, nil
	}
	fd, err := unix.Dup(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	state, err := term.MakeRaw(fd)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	u := &terminalUI{input: in, output: output, state: state, control: fd, resize: make(chan os.Signal, 1), failures: make(chan error, 1)}
	u.viewerDisabled = !viewerTerminal(getenv("TERM"))
	u.initEditor()
	signal.Notify(u.resize, unix.SIGWINCH)
	if err = u.size(); err != nil {
		_ = u.close()
		return nil, err
	}
	u.editor.SetBracketedPasteMode(true)
	return u, nil
}
func (u *terminalUI) size() error {
	width, height, err := term.GetSize(u.control)
	if err != nil {
		return err
	}
	if err := u.editor.SetSize(width, height); err != nil {
		return err
	}
	u.width = width
	return nil
}
func (u *terminalUI) close() error {
	signal.Stop(u.resize)
	// The reader has joined. Remove any still-animated status and leave the
	// shell on a clean line, even if an error interrupted an unsubmitted draft.
	_, viewerErr := u.editor.CloseViewer()
	_ = u.editor.CloseSelection()
	_ = u.editor.SetPromptInfo("", false)
	_ = u.editor.SetStatus(nil)
	_, _ = u.editor.Write(nil)
	_, _ = u.Write([]byte("\x1b[J\r\n"))
	u.editor.SetBracketedPasteMode(false)
	err := errors.Join(viewerErr, term.Restore(u.control, u.state), unix.Close(u.control))
	select {
	case outputErr := <-u.failures:
		err = errors.Join(err, boundary("output terminal", outputErr))
	default:
	}
	return err
}
func (u *terminalUI) Write(p []byte) (int, error) {
	// Each attachment is one library-owned editing cell, shown as a folded
	// block. Both code points encode to three UTF-8 bytes, preserving Write n.
	p = bytes.Map(func(r rune) rune {
		if attachmentRune(r) {
			return 0x25a3
		}
		return r
	}, p)
	n, err := u.output.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		// x/term intentionally ignores some echo-write errors. Make these fatal
		// to the application too, so a broken terminal cannot strand running tools.
		u.once.Do(func() { u.failures <- err; _ = u.input.Close() })
	}
	return n, err
}

// Read recognizes only bracketed-paste framing and Ctrl-C, leaving editing to
// x/term. A paste is never fed to its key parser: short printable text is inserted
// by a callback; long/multiline/control-bearing text becomes an attachment.
func (u *terminalUI) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(u.pending) == 0 {
		// Resolve standalone Escape without a second reader or changing file
		// flags/deadlines on the shared terminal description. Other sequences
		// and bracketed paste remain byte-exact and may arrive in fragments.
		if !u.paste && bytes.Equal(u.sequence, []byte{27}) && u.reader.Buffered() == 0 {
			fds := []unix.PollFd{{Fd: int32(u.control), Events: unix.POLLIN}}
			n, err := unix.Poll(fds, 100)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return 0, err
			}
			if n == 0 {
				u.pending = append(u.pending, 27, 27)
				u.sequence = nil
				break
			}
		}
		c, err := u.reader.ReadByte()
		if err != nil {
			if u.paste {
				u.reject("incomplete bracketed paste")
			}
			return 0, err
		}
		if u.paste {
			if len(u.sequence) == 0 && c != 27 {
				u.pasteByte(c)
				continue
			}
			u.sequence = append(u.sequence, c)
			end := []byte("\x1b[201~")
			// Preserve mismatches verbatim, including overlapping ESC prefixes.
			for len(u.sequence) > 0 && !bytes.HasPrefix(end, u.sequence) {
				u.pasteByte(u.sequence[0])
				u.sequence = u.sequence[1:]
			}
			if bytes.Equal(u.sequence, end) {
				u.paste = false
				u.sequence = nil
				u.pending = append(u.pending, 0) // Private callback key, never raw user input.
			}
			continue
		}
		if c == 3 {
			u.sequence = nil
			// Return through ReadLine and its ordinary application handoff.
			// Do not coalesce keys in a signal channel or read past a stop.
			return 0, errInputInterrupt
		}
		if c == 0 {
			continue
		} // Reserved callback key; ordinary NUL was not editable.
		if len(u.sequence) > 0 || c == 27 {
			u.sequence = append(u.sequence, c)
			start := []byte("\x1b[200~")
			if bytes.Equal(u.sequence, start) {
				u.paste = true
				u.sequence = nil
				u.pasted = nil
			} else if !bytes.HasPrefix(start, u.sequence) {
				u.pending = append(u.pending, u.sequence...)
				u.sequence = nil
			}
			continue
		}
		u.pending = append(u.pending, c)
	}
	// Do not read ahead across a key/submission boundary: the callback must
	// consume this attachment before another paste or submitted line is read.
	n := copy(p, u.pending)
	u.pending = u.pending[n:]
	return n, nil
}
func (u *terminalUI) pasteByte(c byte) {
	if u.rejected != "" {
		return
	}
	if len(u.pasted) == maxMessageBytes {
		u.pasted = nil
		u.reject("message exceeds 1 MiB")
		return
	}
	u.pasted = append(u.pasted, c)
}
