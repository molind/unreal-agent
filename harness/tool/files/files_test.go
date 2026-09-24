package files

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type fileContext struct{ specs []operation.Spec }

func (c *fileContext) Submit(spec operation.Spec) operation.ID {
	c.specs = append(c.specs, spec)
	return "file-1"
}
func TestFileTranslatorsArePureAndValidateArguments(t *testing.T) {
	config := Config{Directory: t.TempDir(), BaseDirectory: t.TempDir()}
	for _, test := range []struct {
		name, args string
		valid      bool
	}{
		{"Read", `{"path":"not-created.txt"}`, true},
		{"Edit", `{"path":"a","revision":"0123456789abcdef","old_text":"old","new_text":""}`, true},
		{"Write", `{"path":"new","revision":"missing","content":""}`, true},
		{"Read", `{"path":"a","limit":0}`, false},
		{"Read", `{"path":"a","command":"rm files"}`, false},
		{"Edit", `{"path":"a","revision":"0123456789abcdef","old_text":"old"}`, false},
		{"Edit", `{"path":"a","revision":"missing","old_text":"old","new_text":"new"}`, false},
		{"Write", `{"path":"a","content":"new"}`, false},
		{"Write", `{"path":"a","revision":"missing","content":null}`, false},
	} {
		t.Run(test.name+test.args, func(t *testing.T) {
			trans := map[string]tool.Translator{"Read": NewRead(config), "Edit": NewEdit(config), "Write": NewWrite(config)}[test.name]
			ctx := &fileContext{}
			status := trans.Translate(ctx, llm.ToolCall{Name: test.name, Arguments: test.args})
			if (status.Error == "") != test.valid {
				t.Fatal(status.Error)
			}
			if test.valid {
				if len(ctx.specs) != 1 || ctx.specs[0].Type != operation.TypeFile {
					t.Fatal("not a structured file operation")
				}
				var state operation.FileState
				if err := json.Unmarshal(ctx.specs[0].State, &state); err != nil {
					t.Fatal(err)
				}
				if !filepath.IsAbs(state.Input.Path) || state.Input.Action != test.name {
					t.Fatal("invalid spec")
				}
			} else if len(ctx.specs) != 0 {
				t.Fatal("invalid call dispatched")
			}
		})
	}
}
