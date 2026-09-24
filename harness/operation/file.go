package operation

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/primitives"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

const (
	TypeFile          Type    = "file"
	VersionFile       Version = 1
	MaxFileBytes              = 8 << 20
	MaxFileInputBytes         = 1 << 20
	FilePreviewLimit          = 16000
)

type FileInput struct {
	Action        string
	Path          string
	BaseDirectory string // Private, session-owned receipts and revision bindings.
	Offset        int
	Limit         int
	Revision      string // Short opaque token from Read, or "missing" for create-only Write.
	OldText       string
	NewText       string
	Content       string
	ReplaceAll    bool
}
type FileResult struct {
	Unit         string `json:"unit,omitempty"`
	Encoding     string `json:"encoding,omitempty"`
	TotalBytes   int64  `json:"total_bytes,omitzero"`
	Path         string `json:"path"`
	Revision     string `json:"revision,omitempty"`
	Text         string `json:"text,omitempty"`
	Offset       int    `json:"offset,omitzero"`
	NextOffset   int    `json:"next_offset,omitzero"`
	TotalLines   int    `json:"total_lines"`
	Replacements int    `json:"replacements,omitzero"`
	Diff         string `json:"diff,omitempty"`
	DiffPath     string `json:"diff_path,omitempty"`
	Truncated    bool   `json:"truncated,omitzero"`
	Created      bool   `json:"created,omitzero"`
	Applied      bool   `json:"applied,omitzero"`
	Recovered    bool   `json:"recovered,omitzero"`
	Error        string `json:"error,omitempty"`
}
type FileState struct {
	Input  FileInput
	Result *FileResult
}

func NewFileSpec(input FileInput) (Spec, error) {
	if err := validateFileInput(input); err != nil {
		return Spec{}, err
	}
	state, err := json.Marshal(FileState{Input: input})
	return Spec{Type: TypeFile, Version: VersionFile, State: state}, err
}
func validateFileInput(in FileInput) error {
	if storage.IsReference(in.Path) && in.Action != "Read" {
		return fmt.Errorf("artifacts are immutable; export before editing")
	}
	if in.Path == "" || (!strings.HasPrefix(in.Path, "/") && !storage.IsReference(in.Path)) || strings.ContainsRune(in.Path, 0) {
		return fmt.Errorf("file path must be absolute and contain no NUL")
	}
	if in.BaseDirectory == "" || !strings.HasPrefix(in.BaseDirectory, "/") {
		return fmt.Errorf("private file operation directory must be absolute")
	}
	switch in.Action {
	case "Read":
		if in.Offset < 1 || in.Limit < 1 || in.Limit > 2000 {
			return fmt.Errorf("Read offset must be >= 1 and limit 1..2000")
		}
	case "Edit", "Write":
		if in.Revision != "missing" {
			if len(in.Revision) != 16 || strings.Trim(in.Revision, "0123456789abcdef") != "" {
				return fmt.Errorf("revision must be the 16-character token from Read")
			}
		} else if in.Action != "Write" {
			return fmt.Errorf("Edit requires a revision from Read")
		}
		for _, text := range []string{in.OldText, in.NewText, in.Content} {
			if len(text) > MaxFileInputBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
				return fmt.Errorf("file text must be valid UTF-8 without NUL and at most 1 MiB per field")
			}
		}
		if in.Action == "Edit" && (in.OldText == "" || in.OldText == in.NewText) {
			return fmt.Errorf("Edit old_text must be nonempty and different from new_text")
		}
	default:
		return fmt.Errorf("unsupported file action %q", in.Action)
	}
	return nil
}
func DecodeFileState(op Operation) (FileState, error) {
	if op.Type != TypeFile || op.Version != VersionFile {
		return FileState{}, fmt.Errorf("file operation version: %w", ErrUnsupported)
	}
	var state FileState
	if err := json.Unmarshal(op.State, &state); err != nil {
		return state, err
	}
	return state, validateFileInput(state.Input)
}

// File actions are dispatched to a joined blocking worker. Translation and the
// coordinator remain I/O-free; only the worker performs filesystem operations.
func AdvanceFile(op Operation, event *primitives.PrimitiveEvent) (Step, error) {
	return advanceFileStored(op, event, nil)
}
func advanceFileStored(op Operation, event *primitives.PrimitiveEvent, db *storage.DB) (Step, error) {
	state, err := DecodeFileState(op)
	if err != nil {
		return Step{}, err
	}
	finish := func(status Status, result FileResult) (Step, error) {
		state.Result = &result
		op.Status = status
		op.State, err = json.Marshal(state)
		return Step{Operation: &op}, err
	}
	if op.Status == StatusCanceling || (event != nil && event.Type == primitives.PrimitiveEventCanceled) {
		return finish(StatusCanceled, FileResult{Error: "file operation canceled; an already committed change is not rolled back. Read the file to verify."})
	}
	if event == nil && (op.Status == StatusReady || op.Status == StatusAwaiting) {
		op.Status = StatusAwaiting
		return Step{Operation: &op, Dispatches: []PrimitiveDispatch{{Type: primitives.PrimitiveDispatchCompute, Data: primitives.ComputeRequest{
			Source: primitives.SourceID(op.ID), CorrelationID: "file",
			Run: func(ctx context.Context) (any, error) {
				result, err := executeFileStored(ctx, op.ID, state.Input, db)
				if err != nil {
					result.Error = err.Error()
				}
				return result, nil
			},
		}}}}, nil
	}
	if event == nil || event.Source != primitives.SourceID(op.ID) {
		return Step{}, fmt.Errorf("invalid file operation event")
	}
	if event.Type == primitives.PrimitiveEventFailed {
		failure, ok := event.Result.(primitives.PrimitiveFailureResult)
		if !ok {
			return Step{}, fmt.Errorf("invalid file failure")
		}
		return finish(StatusFailed, FileResult{Error: failure.Error})
	}
	if event.Type != primitives.PrimitiveEventComputeCompleted || event.CorrelationID != "file" {
		return Step{}, fmt.Errorf("unexpected file event %q", event.Type)
	}
	computed, ok := event.Result.(primitives.ComputeResult)
	if !ok {
		return Step{}, fmt.Errorf("invalid file computation")
	}
	result, ok := computed.Value.(FileResult)
	if !ok {
		return Step{}, fmt.Errorf("invalid file result")
	}
	status := StatusCompleted
	if result.Error != "" {
		status = StatusFailed
	}
	return finish(status, result)
}
