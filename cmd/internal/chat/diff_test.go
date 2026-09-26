package chat

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

// Expected runs alternate between dark and bright, starting with the sign.
// A wholly changed row is uniform, including its sign.
func diffTestRow(parts ...string) string {
	if len(parts) == 2 && len(parts[0]) == 1 {
		parts = []string{parts[0] + parts[1]}
	}
	base, strong := diffRemovedStyle, diffRemovedTextStyle
	if parts[0][0] == '+' {
		base, strong = diffAddedStyle, diffAddedTextStyle
	}
	var out strings.Builder
	for i, part := range parts {
		style := base
		if i%2 == 1 {
			style = strong
		}
		out.WriteString("\x1b[" + style + "m" + part)
	}
	return out.String() + "\x1b[0m"
}

func TestDiffHighlightsOnlyChangedText(t *testing.T) {
	for _, tt := range []struct {
		name, source string
		want         []string
	}{
		{
			name:   "replacement",
			source: "-let n = 1;\n+let n = 20;",
			want:   []string{diffTestRow("-let n = ", "1", ";"), diffTestRow("+let n = ", "20", ";")},
		},
		{
			name:   "separate edits",
			source: "-f(1, keep, 3)\n+f(2, keep, 4)",
			want:   []string{diffTestRow("-f(", "1", ", keep, ", "3", ")"), diffTestRow("+f(", "2", ", keep, ", "4", ")")},
		},
		{
			name:   "insertion",
			source: "-name()\n+name(value)",
			want:   []string{diffTestRow("-name()"), diffTestRow("+name(", "value", ")")},
		},
		{
			name:   "deletion",
			source: "-name(value)\n+name()",
			want:   []string{diffTestRow("-name(", "value", ")"), diffTestRow("+name()")},
		},
		{
			name:   "unicode",
			source: "-ключ: дом 🐈\n+ключ: дым 🐕",
			want:   []string{diffTestRow("-ключ: ", "дом", " ", "🐈"), diffTestRow("+ключ: ", "дым", " ", "🐕")},
		},
		{
			name:   "inserted row does not shift later matches",
			source: "-alpha = 1\n-omega = 3\n+alpha = 2\n+NEW ROW\n+omega = 4",
			want:   []string{diffTestRow("-alpha = ", "1"), diffTestRow("-omega = ", "3"), diffTestRow("+alpha = ", "2"), diffTestRow("+", "NEW ROW"), diffTestRow("+omega = ", "4")},
		},
		{
			name:   "unchanged row inside a replacement",
			source: "-a = 1\n-unchanged\n-z = 3\n+a = 2\n+unchanged\n+z = 4",
			want:   []string{diffTestRow("-a = ", "1"), diffTestRow("-unchanged"), diffTestRow("-z = ", "3"), diffTestRow("+a = ", "2"), diffTestRow("+unchanged"), diffTestRow("+z = ", "4")},
		},
		{
			name:   "standalone removal",
			source: "-gone",
			want:   []string{diffTestRow("-", "gone")},
		},
		{
			name:   "standalone addition",
			source: "+new",
			want:   []string{diffTestRow("+", "new")},
		},
		{
			name:   "empty rows",
			source: "-\n+",
			want:   []string{diffTestRow("-"), diffTestRow("+")},
		},
		{
			name:   "context separates blocks",
			source: "-same\n context\n+same",
			want:   []string{diffTestRow("-", "same"), " context", diffTestRow("+", "same")},
		},
		{
			name:   "hunks separate blocks",
			source: "@@ -1 +1,0 @@\n-same\n@@ -3,0 +3 @@\n+same",
			want:   []string{"@@ -1 +1,0 @@", diffTestRow("-", "same"), "@@ -3,0 +3 @@", diffTestRow("+", "same")},
		},
		{
			name:   "no newline annotations do not separate a replacement",
			source: "-a = 1\n\\ No newline at end of file\n+a = 2\n\\ No newline at end of file",
			want:   []string{diffTestRow("-a = ", "1"), `\ No newline at end of file`, diffTestRow("+a = ", "2"), `\ No newline at end of file`},
		},
		{
			name:   "successive headerless replacements",
			source: "-abc\n+def\n-def\n+ghi",
			want:   []string{diffTestRow("-", "abc"), diffTestRow("+", "def"), diffTestRow("-", "def"), diffTestRow("+", "ghi")},
		},
		{
			name:   "whitespace is a real edit",
			source: "-\tvalue \n+    value",
			want:   []string{diffTestRow("-", "\t", "value", " "), diffTestRow("+", "    ", "value")},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lines := strings.Split(tt.source, "\n")
			highlightDiff(lines)
			got, want := strings.Join(lines, "\n"), strings.Join(tt.want, "\n")
			if got != want {
				t.Fatalf("highlighting:\n%q\nwant:\n%q", got, want)
			}
			if plain := terminalStyle.ReplaceAllString(got, ""); plain != tt.source {
				t.Fatalf("changed diff text: %q", plain)
			}
		})
	}
}

func TestDiffLargeReplacementIsBounded(t *testing.T) {
	// Long identifiers are single tokens; common edge tokens stay dark.
	before, after := strings.Repeat("a", 10000), strings.Repeat("b", 10000)
	lines := []string{"-prefix " + before + " suffix", "+prefix " + after + " suffix"}
	highlightDiff(lines)
	if lines[0] != diffTestRow("-prefix ", before, " suffix") || lines[1] != diffTestRow("+prefix ", after, " suffix") {
		t.Fatal("large replacement did not preserve common edges")
	}
	// A tiny edit on very long rows should not consume the quadratic budget.
	edge := strings.Repeat("unchanged ", 10000)
	lines = []string{"-" + edge + "1 " + edge, "+" + edge + "2 " + edge}
	highlightDiff(lines)
	if lines[0] != diffTestRow("-"+edge, "1", " "+edge) || lines[1] != diffTestRow("+"+edge, "2", " "+edge) {
		t.Fatal("long common edges obscured the small change")
	}
}

func TestDiffBackgroundsPreserveTextAndLeaveHeadersNeutral(t *testing.T) {
	diff := "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1,4 +1,4 @@\n kept\n-old\n--- removed comment\n-\n+new\n+++ added comment\n+\n" +
		"--- a/second\n+++ b/second\n@@ -10 +10 @@ more context\n-last\n+last\n\\ No newline at end of file\n" +
		"diff --git a/new b/new\n--- /dev/null\n+++ b/new\n@@ -0,0 +1 @@\n+created\n"
	for _, color := range []bool{false, true} {
		for _, language := range []string{"diff", "patch", "DIFF"} {
			t.Run(fmt.Sprintf("%s/color=%t", language, color), func(t *testing.T) {
				d := newDisplay(&bytes.Buffer{}, func(string) string { return "" })
				v := markdownView{safe: d.safe, color: color, width: 20}
				got := v.code([]byte(diff), language, "")
				want := "```" + language + "\n" + diff + "```\n\n"
				if plain := terminalStyle.ReplaceAllString(got, ""); plain != want {
					t.Fatalf("changed copied diff:\n%q\nwant:\n%q", plain, want)
				}
				for _, row := range strings.Split(got, "\n") {
					plain := terminalStyle.ReplaceAllString(row, "")
					style := ""
					switch plain {
					case "-old", "--- removed comment", "-", "-last":
						style = diffRemovedStyle
					case "+new", "+++ added comment", "+", "+last", "+created":
						style = diffAddedStyle
					}
					if color && style != "" && (!strings.HasPrefix(row, "\x1b["+style+"m") || !strings.HasSuffix(row, "\x1b[0m")) {
						t.Errorf("missing dark background/reset on %q", plain)
					}
				}
				for _, line := range []string{"--- a/f", "+++ b/f", "--- a/second", "+++ b/second", "--- /dev/null", "+++ b/new", " kept", "@@ -1,4 +1,4 @@", "\\ No newline at end of file"} {
					if !strings.Contains(got, "\n"+line+"\n") {
						t.Errorf("metadata/context row was colored: %q", line)
					}
				}
				if !color && strings.ContainsRune(got, 27) {
					t.Fatal("ANSI added in no-color mode")
				}
			})
		}
	}
}

func TestMarkdownDiffFragmentsOnlyColorDiffBlocks(t *testing.T) {
	d := newDisplay(&bytes.Buffer{}, func(string) string { return "" })
	d.ui, d.color = &terminalUI{width: 24}, true
	fragment := "-\tлік = 1\n+\tлік = 2\n"
	for _, language := range []string{"diff", "patch", "DIFF", "go", ""} {
		got := d.markdown("```" + language + "\n" + fragment + "```\n\nAfter the diff.")
		wantBackground := language == "diff" || language == "patch" || language == "DIFF"
		if strings.Contains(got, diffTestRow("-    лік = ", "1")+"\n") != wantBackground || strings.Contains(got, diffTestRow("+    лік = ", "2")+"\n") != wantBackground {
			t.Fatalf("incorrect background for language %q: %q", language, got)
		}
		if !strings.HasSuffix(got, "After the diff.\n") {
			t.Fatal("style leaked into following prose")
		}
	}
}

func TestDiffFormattingDoesNotChangePlainOutputOrCanonicalResult(t *testing.T) {
	diff := "--- source\n+++ source\n@@ -1 +1 @@\n-old\n+new\n"
	state := operation.FileState{
		Input:  operation.FileInput{Action: "Edit", Path: "/source", BaseDirectory: "/state", Revision: "0123456789abcdef", OldText: "old", NewText: "new"},
		Result: &operation.FileResult{Diff: diff},
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{Type: operation.TypeFile, Version: operation.VersionFile, Status: operation.StatusCompleted, State: encoded}
	var out bytes.Buffer
	d := newDisplay(&out, func(string) string { return "" })
	d.color = true // Even a forced flag must not color the non-TTY file path.
	if err := d.fileChange(op); err != nil {
		t.Fatal(err)
	}
	if out.String() != diff+"\n" {
		t.Fatalf("plain file diff changed: %q", out.String())
	}
	out.Reset()
	d.ui = &terminalUI{width: 80}
	if err := d.fileChange(op); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), diffTestRow("-", "old")+"\n") || !strings.Contains(out.String(), diffTestRow("+", "new")+"\n") {
		t.Fatal("structured file diff did not use the shared background renderer")
	}
	decoded, err := operation.DecodeFileState(op)
	if err != nil || decoded.Result.Diff != diff || !bytes.Equal(encoded, op.State) {
		t.Fatal("presentation mutated the canonical result", err)
	}
}
