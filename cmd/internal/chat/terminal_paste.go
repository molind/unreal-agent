package chat

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
)

const (
	maxMessageBytes     = 1024 * 1024
	maxInlinePasteRunes = 160
	// x/term checks this AFTER AutoCompleteCallback. Guard it there so its
	// silent insertion cap can never turn a long draft into a partial message.
	maxEditorRunes  = 4096
	attachmentFirst = 0xe000
	attachmentLast  = 0xf8ff
)

func attachmentRune(r rune) bool { return r >= attachmentFirst && r <= attachmentLast }

// Use the library history and editing state, but remember lines only AFTER
// validation. Recalled lines contain the same atomic attachment cells, not
// expanded multiline text that would be unsafe to echo or too large to edit.
// Short printable pastes use normal editor text and the same validated history.
type validatedHistory struct{ lineeditor.History }

func (validatedHistory) Add(string) {}

func (u *terminalUI) initEditor() {
	u.reader = bufio.NewReader(u.input)
	u.attachments = make(map[rune]string)
	u.nextAttachment = attachmentFirst
	u.editor = lineeditor.NewTerminal(u, "you> ")
	u.history = u.editor.History
	u.editor.History = validatedHistory{u.history}
	u.editor.AutoCompleteCallback = u.editKey
}
func (u *terminalUI) reject(reason string) {
	if u.rejected != "" {
		return
	}
	u.rejected = reason
	// This runs only in Read or AutoCompleteCallback, where x/term releases its
	// render lock. The ordinary terminal writer still serializes async output.
	_, _ = fmt.Fprintf(u.editor, "Input rejected: %s. Nothing from this draft will be sent; press Enter to start a fresh draft.\n", reason)
}
func (u *terminalUI) editKey(text string, pos int, key rune) (string, int, bool) {
	if key == 0 {
		payload := string(u.pasted)
		u.pasted = nil
		if u.rejected != "" {
			return "", 0, true
		}
		if !utf8.ValidString(payload) {
			u.reject("paste is not valid UTF-8")
			return "", 0, true
		}
		if payload == "" {
			return text, pos, true
		}
		inline := inlinePaste(payload)
		cells := 1
		if inline {
			cells = utf8.RuneCountInString(payload)
		}
		if utf8.RuneCountInString(text)+cells > maxEditorRunes {
			u.reject("editable draft exceeds 4096 cells; use a folded paste for long text")
			return "", 0, true
		}
		if u.expandedSize(text)+len(payload) > maxMessageBytes {
			u.reject("message exceeds 1 MiB")
			return "", 0, true
		}
		if inline {
			// Insert as data in one callback, never through the key parser.
			// The editor owns character editing and history for short pastes.
			return text[:pos] + payload + text[pos:], pos + len(payload), true
		}
		// Reuse only unreferenced slots; live draft/history references are retained.
		for attempts := 0; attempts <= attachmentLast-attachmentFirst; attempts++ {
			marker := u.nextAttachment
			u.nextAttachment++
			if u.nextAttachment > attachmentLast {
				u.nextAttachment = attachmentFirst
			}
			if _, used := u.attachments[marker]; used {
				continue
			}
			u.attachments[marker] = payload
			_, _ = fmt.Fprintf(u.editor, "Paste attached as ▣: %d bytes, %d lines. Exact text retained; Backspace/Delete removes the block.\n", len(payload), strings.Count(payload, "\n")+1)
			return text[:pos] + string(marker) + text[pos:], pos + utf8.RuneLen(marker), true
		}
		u.reject("too many retained paste attachments")
		return "", 0, true
	}
	if key == lineeditor.KeyNewline {
		if u.rejected != "" {
			return "", 0, true
		}
		if utf8.RuneCountInString(text) >= maxEditorRunes {
			u.reject("editable draft exceeds 4096 cells")
			return "", 0, true
		}
		if u.expandedSize(text)+1 > maxMessageBytes {
			u.reject("message exceeds 1 MiB")
			return "", 0, true
		}
		return text[:pos] + "\n" + text[pos:], pos + 1, true
	}
	if attachmentRune(key) {
		u.reject("private attachment markers cannot be typed; paste this character instead")
		return "", 0, true
	}
	// Mirror the library printable-key contract; navigation/editing keys and
	// ignored controls must not be mistaken for discarded text.
	if key < 32 || (key >= 0xd800 && key <= 0xdbff) {
		return "", 0, false
	}
	if u.rejected != "" {
		return "", 0, true
	}
	if utf8.RuneCountInString(text) >= maxEditorRunes {
		u.reject("editable draft exceeds 4096 cells; use a folded paste for long text")
		return "", 0, true
	}
	if u.expandedSize(text)+utf8.RuneLen(key) > maxMessageBytes {
		u.reject("message exceeds 1 MiB")
		return "", 0, true
	}
	return "", 0, false
}

// Long/multiline or control-bearing payloads stay folded and byte-exact.
// Short printable text is safe to display and edit like ordinary typed text.
func inlinePaste(payload string) bool {
	if utf8.RuneCountInString(payload) > maxInlinePasteRunes {
		return false
	}
	for _, r := range payload {
		if !unicode.IsPrint(r) || attachmentRune(r) {
			return false
		}
	}
	return true
}

func (u *terminalUI) expandedSize(text string) int {
	size := len(text)
	for _, r := range text {
		if payload, ok := u.attachments[r]; ok {
			size += len(payload) - utf8.RuneLen(r)
		}
	}
	return size
}
func (u *terminalUI) readLine() (line, error) {
	for {
		text, err := u.editor.ReadLine()
		if errors.Is(err, errInputInterrupt) {
			cleared, clearErr := u.editor.ClearDraft()
			if clearErr != nil {
				return line{}, clearErr
			}
			cleared = cleared || u.rejected != ""
			u.rejected = ""
			u.pasted = nil
			u.pruneAttachments()
			return line{interrupt: true, cleared: cleared}, nil
		}
		if err != nil {
			var selected *lineeditor.Selection
			if errors.As(err, &selected) {
				u.pasted = nil // A paste in a menu is discarded, never a draft.
				u.rejected = ""
				return line{selection: selected}, nil
			}
			return line{}, err
		}
		if u.rejected != "" {
			u.rejected = ""
			u.pruneAttachments()
			continue // Rejected drafts are neither submitted nor put into history.
		}
		result := line{}
		var expanded strings.Builder
		for _, r := range text {
			if payload, ok := u.attachments[r]; ok {
				expanded.WriteString(payload)
				result.literal = true
			} else {
				expanded.WriteRune(r)
			}
		}
		result.text = expanded.String()
		// Multiline pasted code must never become a chat command. Single-line
		// commands, whether typed or pasted, still run only on an explicit Enter
		// outside paste framing (including /resume plus a copied ID).
		// A copied command/ID may include a trailing newline or blank padding;
		// only multiple content lines force literal user-message semantics.
		commandLine := strings.TrimSpace(result.text)
		if result.literal && isCommand(commandLine) {
			result.literal = false
		}
		if strings.ContainsRune(text, '\n') {
			result.literal = true // Explicit Option+Return is content, not a command.
		}

		u.history.Add(text)
		u.pruneAttachments()
		return result, nil
	}
}
func (u *terminalUI) pruneAttachments() {
	used := make(map[rune]bool)
	for i := 0; i < u.history.Len(); i++ {
		for _, r := range u.history.At(i) {
			if attachmentRune(r) {
				used[r] = true
			}
		}
	}
	for marker := range u.attachments {
		if !used[marker] {
			delete(u.attachments, marker)
		}
	}
}
