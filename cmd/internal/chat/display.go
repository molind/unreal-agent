package chat

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[^\s";]+`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
	regexp.MustCompile(`(?i)"?[A-Z0-9_]*(?:TOKEN|PASSWORD|PASSWD|API[_-]?KEY|SECRET|AUTHORIZATION|ACCOUNT_ID)"?\s*[:=]\s*(?:"[^"]*"|[^\s,;&]+)`),
	regexp.MustCompile(`(?i)https?://[^/\s@]+@`),
}

type operationNotice struct {
	state, description string
	started            time.Time
}
type display struct {
	out        io.Writer
	secrets    []string
	operations map[operation.ID]operationNotice
	calls      map[string]string
	generating bool
	started    time.Time
	color      bool
	ui         *terminalUI
}

func newDisplay(out io.Writer, getenv func(string) string) *display {
	d := &display{out: out}
	// Only explicitly named authentication values are consulted, never the full
	// environment or auth files. Raw exchanges and tool output are never displayed.
	for _, name := range []string{"UNREAL_HARNESS_LLM_API_KEY", "OPENAI_API_KEY", "OPENAI_CODEX_ACCESS_TOKEN", "OPENAI_CODEX_ACCOUNT_ID", "OPENROUTER_API_KEY", "FIREWORKS_API_KEY"} {
		if v := getenv(name); v != "" {
			d.secrets = append(d.secrets, v)
		}
	}
	d.reset()
	return d
}
func (d *display) reset() {
	d.operations = make(map[operation.ID]operationNotice)
	d.calls = make(map[string]string)
	d.generating = false
}
func (d *display) safe(s string) string {
	for _, secret := range d.secrets {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	for _, pattern := range credentialPatterns {
		s = pattern.ReplaceAllString(s, "[redacted]")
	}
	return strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) && r != 10 && r != 9) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
func (d *display) print(format string, args ...any) error {
	text := d.safe(fmt.Sprintf(format, args...))
	return d.write(text)
}
func (d *display) write(text string) error {
	n, err := io.WriteString(d.out, text)
	if err == nil && n != len(text) {
		return io.ErrShortWrite
	}
	return err
}
func (d *display) item(item sessionstore.Item, replay bool) error {
	switch v := item.Data.(type) {
	case inbox.Input:
		if v.Kind == inbox.InputExternal && replay {
			var text string
			if err := json.Unmarshal(v.Payload, &text); err != nil {
				return err
			}
			return d.message("you", text)
		}
	case sessionstore.ModelResponse:
		d.generating = false
		for _, output := range v.Response.Output {
			switch value := output.Data.(type) {
			case llm.Message:
				if output.Type == llm.ItemMessage && value.Role == llm.RoleAssistant {
					if err := d.message("assistant", value.Text); err != nil {
						return err
					}
				}
			case llm.ToolCall:
				var args struct {
					Command string `json:"command"`
					Path    string `json:"path"`
				}
				_ = json.Unmarshal([]byte(value.Arguments), &args)
				desc := value.Name
				if args.Command != "" {
					desc += ": " + args.Command
				} else if args.Path != "" {
					desc += ": " + args.Path
				}
				d.calls[string(v.TurnID)+"/"+value.CallID] = desc
			}
		}
	case sessionstore.ToolCallStatus:
		desc := d.calls[string(v.TurnID)+"/"+v.CallID]
		if !replay && v.Status.Error != "" {
			var err error
			if d.ui != nil {
				err = d.write("  " + paint(d.color, "31", "✗") + " " + commandSummary(d.safe(desc), max(12, d.columns()-5)) + "\n" + wrapProse(d.safe("failed: "+v.Status.Error), "    ", d.columns()-1))
			} else {
				err = d.print("tool call %s failed: %s (%s)\n", v.CallID, desc, v.Status.Error)
			}
			if err != nil {
				return err
			}
		}
		for _, op := range v.Operations {
			if err := d.operation(op, desc, replay); err != nil {
				return err
			}
		}
	}
	if item.Kind == sessionstore.ItemTurn && !replay {
		return d.working()
	}
	return d.tick(0)
}
func (d *display) operation(op operation.Operation, desc string, replay bool) error {
	old, exists := d.operations[op.ID]
	if desc == "" {
		desc = old.description
	}
	if desc == "" {
		desc = string(op.Type)
	}
	// A durable ready checkpoint is not yet owned by the manager. This also
	// applies during replay: do not expose a cancellable ID before Add.
	if op.Status == operation.StatusReady {
		d.operations[op.ID] = operationNotice{description: desc}
		return nil
	}
	state := operationState(op)
	started := old.started
	if started.IsZero() {
		started = time.Now()
	}
	notice := operationNotice{state: state, description: desc, started: started}
	d.operations[op.ID] = notice
	if replay || (exists && old.state == notice.state) {
		return nil
	}
	if err := d.tick(0); err != nil {
		return err
	}
	if d.ui != nil && !terminal(op.Status) {
		return nil // Running/canceling checkpoints are transient on a TTY.
	}
	if d.ui != nil {
		marker, color := "✓", "32"
		switch {
		case strings.HasPrefix(state, "failed"):
			marker, color = "✗", "31"
		case state == "canceled":
			marker, color = "–", "33"
		}
		duration := fmt.Sprintf("%.1fs", time.Since(started).Seconds())
		label := commandSummary(d.safe(desc), max(8, d.columns()-len(duration)-9))
		text := "  " + paint(d.color, color, marker) + " " + label + "  " + paint(d.color, "2", duration) + "\n"
		if state != "completed" {
			id := []rune(d.safe(string(op.ID)))
			details := state + " · " + string(id[:min(8, len(id))]) + " · /status for details"
			text += paint(d.color, "2", wrapProse(details, "    ", d.columns()-1))
		}
		return d.write(text)
	}
	// Preserve the plain/piped lifecycle format, including exact operation IDs.
	desc = d.safe(strings.Join(strings.Fields(desc), " "))
	desc = clipText(desc, 240)
	return d.print("tool %s %s — %s (%.1fs)\n", op.ID, state, desc, time.Since(started).Seconds())
}

// Short headings retain the first command line rather than flattening an entire
// heredoc into the transcript. Full descriptions remain available in /status.
func commandSummary(description string, limit int) string {
	lines := strings.Split(strings.TrimSpace(description), "\n")
	head := strings.Join(strings.Fields(lines[0]), " ")
	if len(lines) == 1 {
		return clipText(head, limit)
	}
	tail := fmt.Sprintf(" · +%d lines", len(lines)-1)
	if len([]rune(tail))+4 >= limit {
		return clipText(head, limit)
	}
	return clipText(head, limit-len([]rune(tail))) + tail
}

func clipText(text string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	if width <= 0 {
		return ""
	}
	return string(runes[:width-1]) + "…"
}

func (d *display) columns() int {
	if d.ui != nil && d.ui.width > 0 {
		return d.ui.width
	}
	return 80
}

func (d *display) message(role, body string) error {
	if d.ui == nil {
		return d.print("%s> %s\n", role, body)
	}
	if role == "you" {
		return d.write("\n" + paint(d.color, "1;36", "you> ") + d.safe(body) + "\n\n")
	}
	return d.write("\n" + paint(d.color, "2", "  Assistant") + "\n" + d.markdown(body) + "\n")
}

func (d *display) status() error {
	state := "idle"
	if d.generating {
		state = "model generating / input pending"
	}
	if err := d.print("Status: %s\n", state); err != nil {
		return err
	}
	ids := make([]string, 0, len(d.operations))
	for id, notice := range d.operations {
		if notice.state == "" {
			continue
		}
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := d.operations[operation.ID(id)]
		if err := d.print("  %s %s — %s\n", id, n.state, n.description); err != nil {
			return err
		}
	}
	return nil
}

func operationState(op operation.Operation) string {
	state := "running"
	if terminal(op.Status) || op.Status == operation.StatusCanceling {
		state = string(op.Status)
	}
	if op.Type == operation.TypeShell && op.Status == operation.StatusCompleted {
		if shell, err := operation.DecodeShellState(op); err == nil && shell.Result != nil && shell.Result.ExitCode != 0 {
			state = fmt.Sprintf("failed (exit %d)", shell.Result.ExitCode)
		}
	}
	return state
}
func (d *display) working() error {
	if d.generating {
		return nil
	}
	d.generating = true
	d.started = time.Now()
	if d.ui != nil {
		return d.tick(0)
	}
	return d.print("Working — waiting for model response. Ctrl-C or /stop to stop.\n")
}
func (d *display) tick(frame int) error {
	if d.ui == nil {
		return nil
	}
	var rows []string
	frames := []string{"*", "+", "-", "."}
	if d.generating {
		rows = append(rows, fmt.Sprintf("%s model / input pending %.0fs (Ctrl-C or /stop)", frames[frame%4], time.Since(d.started).Seconds()))
	}
	ids := make([]string, 0, len(d.operations))
	for id, n := range d.operations {
		if n.state == "running" || n.state == "canceling" {
			ids = append(ids, string(id))
		}
	}
	sort.Strings(ids)
	for i, id := range ids {
		n := d.operations[operation.ID(id)]
		// ASCII status avoids assuming the terminal cell width of command text.
		// Full Unicode descriptions remain available in /status and completions.
		label := strconv.QuoteToASCII(commandSummary(d.safe(n.description), 120))
		label = label[1 : len(label)-1]
		shortID := strconv.QuoteToASCII(d.safe(id))
		shortID = shortID[1 : len(shortID)-1]
		rows = append(rows, fmt.Sprintf("%s %s %s %.0fs %s", frames[(frame+i)%4], shortID[:min(8, len(shortID))], n.state, time.Since(n.started).Seconds(), label))
	}
	return d.ui.editor.SetStatus(rows)
}
