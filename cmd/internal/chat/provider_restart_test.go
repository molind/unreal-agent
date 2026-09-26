package chat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestProviderRestartKeepsChatOpenWithoutReplayingTools(t *testing.T) {
	workspace := t.TempDir()
	c := launch(t, workspace, false)
	c.send("do this once")
	call := c.call()
	call.reply <- shellResponse("printf once >> executed.txt")
	c.wait("completed")
	followup := c.call()
	followup.failure <- fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: websocket.StatusServiceRestart})
	c.wait("Provider restarted; work stopped.")
	select {
	case <-c.client.calls:
		t.Fatal("runtime failure automatically retried the model")
	case <-time.After(100 * time.Millisecond):
	}
	c.send("/status")
	c.wait("Status: idle")
	c.wait("Diagnostic log:")
	c.send("continue")
	continued := c.call()
	users := messages(continued.request, llm.RoleUser)
	if len(users) != 2 || users[0] != "do this once" || users[1] != "continue" {
		t.Fatalf("user history was lost on provider restart: %v", users)
	}
	continued.reply <- reply("continued successfully")
	c.wait("continued successfully")
	c.finish("/exit")
	data, err := os.ReadFile(filepath.Join(workspace, "executed.txt"))
	if err != nil || string(data) != "once" {
		t.Fatalf("completed tool repeated or lost: %q, %v", data, err)
	}
	logs, err := filepath.Glob(filepath.Join(workspace, ".harness/sessions/logs/diagnostic-*.jsonl"))
	if err != nil || len(logs) != 1 {
		t.Fatal("missing diagnostics", err)
	}
	found := false
	for _, record := range records(t, logs[0]) {
		if record.Stage == "provider" && record.Event == "request_failed" && strings.Contains(record.Error, "StatusServiceRestart") && record.Stack != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("recoverable provider failure was not logged")
	}
}

func TestProviderRestartClassificationPreservesOtherFailures(t *testing.T) {
	restart := websocket.CloseError{Code: websocket.StatusServiceRestart}
	for _, err := range []error{restart, &restart, fmt.Errorf("provider: %w", errors.Join(fmt.Errorf("reader: %w", restart)))} {
		if !recoverableProviderRestart(err) {
			t.Fatalf("lost typed provider restart: %v", err)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("StatusServiceRestart"),
		websocket.CloseError{Code: websocket.StatusPolicyViolation},
		errors.Join(restart, errors.New("disk full")),
		fmt.Errorf("runtime: %w", errors.Join(restart, errors.New("output failed"))),
		&diagnosticWriteError{restart},
		errors.Join(&diagnosticWriteError{errors.Join(restart)}),
	} {
		if recoverableProviderRestart(err) {
			t.Fatalf("hidden nonrecoverable cause: %v", err)
		}
	}
}
