package tool

import "github.com/unreallabsai/unreal-agent/harness/llm"

func fileDefinitions() []Definition {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	path := func() map[string]any {
		return str("UTF-8 text file path, absolute or workspace-relative. Symlink targets and non-regular files are rejected.")
	}
	revision := func() map[string]any {
		return str("16-character revision from the latest Read/Edit/Write of this file. Read again after a conflict. Write may use 'missing' to create only if absent.")
	}
	makeTool := func(name, description string, properties map[string]any, required ...any) Definition {
		return Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: name, Description: description, Parameters: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}}
	}
	return []Definition{
		makeTool(ReadName, "Read a local UTF-8 text file with numbered lines and a short revision token. Prefer this to shell file reads. Reads up to 8 MiB; output is bounded. Not a sandbox.", map[string]any{
			"path": path(), "offset": map[string]any{"type": "integer", "minimum": 1, "default": 1, "description": "First line, 1-based."}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 2000, "default": 200},
		}, "path"),
		makeTool(EditName, "Edit a file by exact literal text replacement, with revision conflict detection. Prefer this to shell/Python edits. Unique old_text is required unless replace_all=true. Returns a diff and next revision. User authorization to edit is still required.", map[string]any{
			"path": path(), "revision": revision(), "old_text": str("Exact nonempty text to replace; include enough context to be unique."), "new_text": str("Replacement text; empty deletes old_text."), "replace_all": map[string]any{"type": "boolean", "default": false},
		}, "path", "revision", "old_text", "new_text"),
		makeTool(WriteName, "Create or explicitly replace an entire UTF-8 text file. Use revision='missing' for create-only, otherwise Read first and supply its revision. Prefer Edit for targeted changes. Parent directory must exist. Returns a diff and next revision.", map[string]any{
			"path": path(), "revision": revision(), "content": str("Complete new file contents; may be empty. Maximum 1 MiB."),
		}, "path", "revision", "content"),
	}
}
