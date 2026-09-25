package chat

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
)

// Alternate-screen controls are an xterm-family extension, not plain VT100.
// Unknown terminal descriptions keep ordinary report output rather than risk
// clearing a primary screen on an emulator that ignores alternate-buffer entry.
func viewerTerminal(term string) bool {
	for _, prefix := range []string{"xterm", "screen", "tmux", "rxvt", "alacritty", "kitty", "wezterm", "foot", "iterm", "st-", "putty"} {
		if strings.HasPrefix(term, prefix) {
			return true
		}
	}
	return false
}

// Reports are snapshots of application-owned display state. Building one does
// not change the live renderer, session history, model input or tool scheduling.
// Plain/piped output remains ordinary text, without modal controls.
func (a *application) report(title string, render func(*application) error) error {
	if a.display.ui == nil || a.display.ui.viewerDisabled {
		return render(a)
	}
	output := &reportBuffer{}
	display := *a.display
	display.out, display.ui, display.color = output, nil, false
	view := *a
	view.display = &display
	if err := render(&view); err != nil && !errors.Is(err, lineeditor.ErrViewerTooLarge) {
		return err
	}
	text := output.String()
	if output.truncated {
		text += "\n[Report stopped at the 1 MiB viewer limit; remaining entries are not shown.]\n"
	}
	opened, err := a.display.ui.editor.OpenViewer(title, text)
	if err != nil {
		return err
	}
	if !opened {
		return a.display.print("Close the current menu with Esc before opening a report.\n")
	}
	return nil
}

type reportBuffer struct {
	strings.Builder
	truncated bool
}

func (b *reportBuffer) WriteString(text string) (int, error) { return b.Write([]byte(text)) }
func (b *reportBuffer) Write(p []byte) (int, error) {
	const reserve = 128 // Always leave room for an explicit truncation notice.
	room := lineeditor.ViewerLimit - reserve - b.Len()
	if len(p) <= room {
		return b.Builder.Write(p)
	}
	end := max(0, room)
	for end > 0 && end < len(p) && !utf8.RuneStart(p[end]) {
		end--
	}
	_, _ = b.Builder.Write(p[:end])
	b.truncated = true
	return end, lineeditor.ErrViewerTooLarge
}
