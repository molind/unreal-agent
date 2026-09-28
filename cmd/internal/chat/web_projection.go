package chat

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"strconv"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Web projections intentionally omit provider payloads, reasoning and private
// operation metadata. The same credential/control redactor serves both UIs.
func WebRedactor(getenv func(string) string) func(string) string {
	return newDisplay(io.Discard, getenv).safe
}

type WebItem struct {
	Sequence   sessionstore.Sequence `json:"sequence"`
	Kind       string                `json:"kind"`
	ID         string                `json:"id,omitempty"`
	Turn       string                `json:"turn,omitempty"`
	Text       string                `json:"text,omitempty"`
	HTML       string                `json:"html,omitempty"`
	Operations []WebOperation        `json:"operations,omitempty"`
}

var webMarkdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

type WebOperation struct {
	ApprovalID  string `json:"approval_id,omitempty"`
	Directory   string `json:"directory,omitempty"`
	ID          string `json:"id"`
	State       string `json:"state"`
	Description string `json:"description"`
	Output      string `json:"output,omitempty"`
	Diff        string `json:"diff,omitempty"`
}

func ProjectOperation(op operation.Operation, safe func(string) string) WebOperation {
	v := WebOperation{ID: string(op.ID), State: operationState(op), Description: string(op.Type)}
	switch op.Type {
	case operation.TypeShell:
		if s, err := operation.DecodeShellState(op); err == nil {
			v.Description = s.Input.Command
			v.ApprovalID = operation.ShellApprovalID(op)
			v.Directory = safe(s.Input.Directory)
			v.Output = s.TerminalError
			if s.Result != nil {
				v.Output += s.Result.Out + s.Result.Err
			}
		}
	case operation.TypeFile:
		if s, err := operation.DecodeFileState(op); err == nil {
			v.Description = s.Input.Action + " " + s.Input.Path
			if s.Result != nil {
				v.Output = s.Result.Text + s.Result.Error
				v.Diff = s.Result.Diff
			}
		}
	}
	v.Description = safe(v.Description)
	v.Output = clipText(safe(v.Output), 32000)
	v.Diff = clipText(safe(v.Diff), 32000)
	return v
}

func ProjectItem(item sessionstore.Item, safe func(string) string) ([]WebItem, error) {
	base := WebItem{Sequence: item.Sequence, ID: strconv.FormatUint(uint64(item.Sequence), 10)}
	switch v := item.Data.(type) {
	case inbox.Input:
		if v.Kind == inbox.InputExternal {
			if err := json.Unmarshal(v.Payload, &base.Text); err != nil {
				return nil, err
			}
			base.Kind, base.ID, base.Text = "user", string(v.ID), safe(base.Text)
			return []WebItem{base}, nil
		}
	case session.Turn:
		if v.Type == session.TurnCompaction {
			base.Kind, base.Turn = "compaction", string(v.ID)
			return []WebItem{base}, nil
		}
	case sessionstore.ModelResponse:
		var items []WebItem
		for i, output := range v.Response.Output {
			if msg, ok := output.Data.(llm.Message); ok && output.Type == llm.ItemMessage && msg.Role == llm.RoleAssistant {
				x := base
				x.Kind, x.Turn, x.Text = "assistant", string(v.TurnID), safe(msg.Text)
				x.ID += "/" + strconv.Itoa(i)
				var html bytes.Buffer
				if err := webMarkdown.Convert([]byte(x.Text), &html); err != nil {
					return nil, err
				}
				x.HTML = html.String()
				items = append(items, x)
			}
		}
		return items, nil
	case sessionstore.ToolCallStatus:
		base.Kind, base.ID, base.Turn, base.Text = "tools", string(v.TurnID)+"/"+v.CallID, string(v.TurnID), safe(v.Status.Error)
		for _, op := range v.Operations {
			base.Operations = append(base.Operations, ProjectOperation(op, safe))
		}
		return []WebItem{base}, nil
	}
	return nil, nil
}
