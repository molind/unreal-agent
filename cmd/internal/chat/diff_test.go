package chat

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

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
				for _, line := range []string{"-old", "--- removed comment", "-", "-last"} {
					if !strings.Contains(got, paint(color, diffRemovedStyle, line)+"\n") {
						t.Errorf("missing red background/reset on %q", line)
					}
				}
				for _, line := range []string{"+new", "+++ added comment", "+", "+last", "+created"} {
					if !strings.Contains(got, paint(color, diffAddedStyle, line)+"\n") {
						t.Errorf("missing green background/reset on %q", line)
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
	fragment := "-\tбыло\n+\tстала\n"
	for _, language := range []string{"diff", "go", ""} {
		got := d.markdown("```" + language + "\n" + fragment + "```\n\nAfter the diff.")
		wantBackground := language == "diff"
		if strings.Contains(got, "\x1b[97;41m-    было\x1b[0m\n") != wantBackground || strings.Contains(got, "\x1b[30;42m+    стала\x1b[0m\n") != wantBackground {
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
	if !strings.Contains(out.String(), "\x1b[97;41m-old\x1b[0m\n") || !strings.Contains(out.String(), "\x1b[30;42m+new\x1b[0m\n") {
		t.Fatal("structured file diff did not use the shared background renderer")
	}
	decoded, err := operation.DecodeFileState(op)
	if err != nil || decoded.Result.Diff != diff || !bytes.Equal(encoded, op.State) {
		t.Fatal("presentation mutated the canonical result", err)
	}
}
