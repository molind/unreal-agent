// Package files exposes structured file operations without invoking a shell.
package files

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/storage"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type Config struct{ Directory, BaseDirectory string }
type translator struct {
	action string
	config Config
}

func NewRead(c Config) tool.Translator  { return &translator{action: "Read", config: c} }
func NewEdit(c Config) tool.Translator  { return &translator{action: "Edit", config: c} }
func NewWrite(c Config) tool.Translator { return &translator{action: "Write", config: c} }
func (t *translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	// Action-specific structs reject irrelevant/unknown arguments and distinguish
	// absent strings from intentionally empty replacement/new-file contents.
	in := operation.FileInput{Action: t.action, BaseDirectory: t.config.BaseDirectory}
	var err error
	switch t.action {
	case "Read":
		args := struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}{Offset: 1, Limit: 200}
		err = json.Unmarshal([]byte(call.Arguments), &args, json.RejectUnknownMembers(true))
		in.Path, in.Offset, in.Limit = args.Path, args.Offset, args.Limit
	case "Edit":
		var args struct {
			Path       string  `json:"path"`
			Revision   string  `json:"revision"`
			OldText    *string `json:"old_text"`
			NewText    *string `json:"new_text"`
			ReplaceAll bool    `json:"replace_all"`
		}
		err = json.Unmarshal([]byte(call.Arguments), &args, json.RejectUnknownMembers(true))
		if err == nil && (args.OldText == nil || args.NewText == nil) {
			err = fmt.Errorf("old_text and new_text must both be supplied")
		}
		in.Path, in.Revision, in.ReplaceAll = args.Path, args.Revision, args.ReplaceAll
		if args.OldText != nil {
			in.OldText = *args.OldText
		}
		if args.NewText != nil {
			in.NewText = *args.NewText
		}
	case "Write":
		var args struct {
			Path     string  `json:"path"`
			Revision string  `json:"revision"`
			Content  *string `json:"content"`
		}
		err = json.Unmarshal([]byte(call.Arguments), &args, json.RejectUnknownMembers(true))
		if err == nil && args.Content == nil {
			err = fmt.Errorf("content must be supplied (may be empty)")
		}
		in.Path, in.Revision = args.Path, args.Revision
		if args.Content != nil {
			in.Content = *args.Content
		}
	}
	if err != nil {
		return tool.ErrorStatus(fmt.Sprintf("%s arguments: %v", t.action, err), operation.DefaultMaxOutputLength)
	}
	if strings.TrimSpace(in.Path) == "" {
		return tool.ErrorStatus("path must be nonempty", operation.DefaultMaxOutputLength)
	}
	if !filepath.IsAbs(in.Path) && !storage.IsReference(in.Path) {
		in.Path = t.config.Directory + string(filepath.Separator) + in.Path
	}
	spec, err := operation.NewFileSpec(in)
	if err != nil {
		return tool.ErrorStatus(err.Error(), operation.DefaultMaxOutputLength)
	}
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}
func (t *translator) TranslateResult(callID string, status tool.CallStatus, ops []operation.Operation) (llm.ToolResult, error) {
	result := llm.ToolResult{CallID: callID}
	text := ""
	if status.Error != "" {
		if len(ops) != 0 {
			return result, fmt.Errorf("file call has both validation error and operations")
		}
		text = "Error: " + status.Error
	} else {
		if len(ops) != 1 {
			return result, fmt.Errorf("%s needs exactly one operation", t.action)
		}
		state, err := operation.DecodeFileState(ops[0])
		if err != nil {
			return result, err
		}
		if state.Input.Action != t.action {
			return result, fmt.Errorf("file result action does not match tool")
		}
		if state.Result == nil {
			text = "File operation is still running."
		} else {
			encoded, err := json.Marshal(state.Result)
			if err != nil {
				return result, err
			}
			text = string(encoded)
		}
	}
	result.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}
	return result, nil
}
