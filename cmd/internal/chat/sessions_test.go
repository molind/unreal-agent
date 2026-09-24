package chat

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
)

func durableCount(t *testing.T, workspace string, want int) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(workspace, ".harness/sessions/*.session.jsonl"))
	if err != nil || len(files) != want {
		t.Fatalf("durable sessions=%d want %d (%v)", len(files), want, err)
	}
}

func TestUnsavedLifetimeAndPlainResume(t *testing.T) {
	workspace := t.TempDir()
	c := launch(t, workspace, false)
	durableCount(t, workspace, 0)
	for _, command := range []string{"/status", "/help", "/new", "/resume", "/stop", "/sessions"} {
		c.send(command)
	}
	c.wait("No saved sessions.")
	c.finish("/exit")
	durableCount(t, workspace, 0)
	c = launch(t, workspace, false)
	c.finish("EOF")
	durableCount(t, workspace, 0)
	c = launch(t, workspace, false)
	c.send("first actual message")
	call := c.call()
	assertMessages(t, call.request, llm.RoleUser, "first actual message")
	id := sessionID(t, c.output.snapshot())
	durableCount(t, workspace, 1)
	call.reply <- reply("first answer")
	c.wait("first answer")
	c.finish("/exit")
	c = launch(t, workspace, false)
	c.send("/resume")
	c.wait("Cursor chooser requires a capable terminal")
	c.wait("First prompt: first actual message")
	select {
	case <-c.client.calls:
		t.Fatal("fallback called model")
	default:
	}
	c.send("/resume " + id)
	c.wait("Selected session " + id)
	c.send("/new")
	c.send("/resume " + id)
	c.send("next actual message")
	call = c.call()
	assertMessages(t, call.request, llm.RoleUser, "first actual message", "next actual message")
	assertMessages(t, call.request, llm.RoleAssistant, "first answer")
	call.reply <- reply("continued")
	c.wait("continued")
	c.finish("/exit")
	durableCount(t, workspace, 1)
	c = launch(t, workspace, false, "-session", id)
	c.finish("/exit")
	durableCount(t, workspace, 1)
}

func TestSessionChoicesRecentIdentityErrorsAndCancel(t *testing.T) {
	directory := t.TempDir()
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d := newDisplay(&out, func(string) string { return "" })
	logs, err := openLogs(directory, d.safe)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.file.Close()
	a := &application{ctx: t.Context(), store: store, display: d, logs: logs}
	choices, err := a.sessionChoices()
	if err != nil || len(choices) != 0 {
		t.Fatal(choices, err)
	}
	now := time.Now().Add(-time.Hour)
	for i, id := range []session.ID{"old", "recent", "tie-b", "tie-a"} {
		if _, err := store.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendInput(t.Context(), id, newInput(inbox.InputExternal, "Беларуская тэма\x1b[2J")); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(time.Duration(min(i, 2)) * time.Minute)
		if err := os.Chtimes(filepath.Join(directory, string(id)+".session.jsonl"), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	a.id = "recent"
	choices, err = a.sessionChoices()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, choice := range choices {
		ids = append(ids, choice.Value)
		if !strings.Contains(choice.Label, "Беларуская тэма") || strings.ContainsRune(choice.Label, 27) {
			t.Fatal(choice)
		}
	}
	if strings.Join(ids, ",") != "tie-a,tie-b,recent,old" {
		t.Fatal(ids)
	}
	if !strings.HasPrefix(choices[2].Label, "*") {
		t.Fatal("no current marker")
	}
	if exit, err := a.accept(line{selection: &lineeditor.Selection{Canceled: true}}); exit || err != nil || a.id != "recent" {
		t.Fatal("cancel changed selection", err)
	}
	for _, id := range []string{"tie-a", "tie-b"} {
		path := filepath.Join(directory, id+".session.jsonl")
		if id == "tie-a" {
			err = os.Remove(path)
		} else {
			err = os.WriteFile(path, []byte("corrupt\n"), 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
		if exit, err := a.accept(line{selection: &lineeditor.Selection{Value: id}}); exit || err != nil || a.id != "recent" {
			t.Fatal("invalid selection changed session", err)
		}
	}
	if strings.Count(out.String(), "Current session retained") != 2 {
		t.Fatal(out.String())
	}
	choices, err = a.sessionChoices()
	if err != nil {
		t.Fatal(err)
	}
	for _, choice := range choices {
		if choice.Value == "tie-b" && !strings.Contains(choice.Label, "unreadable") {
			t.Fatal(choice)
		}
	}
	// A listing failure is useful output, not a crash or a fallback selection.
	a.display.ui = &terminalUI{editor: lineeditor.NewTerminal(&out, "you> ")}
	if err := os.Rename(directory, directory+"-moved"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(directory+"-moved", directory)
	if err := a.chooseSession(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Cannot list sessions") {
		t.Fatal(fmt.Sprint(out.String()))
	}
}
