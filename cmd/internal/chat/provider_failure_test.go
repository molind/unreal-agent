package chat

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
)

func TestTransientProviderFailuresKeepHistoryAndTools(t *testing.T) {
	for _, kind := range []string{"API error", "failed response"} {
		t.Run(kind, func(t *testing.T) {
			workspace := t.TempDir()
			c := launch(t, workspace, false)
			c.send("do this once")
			c.call().reply <- shellResponse("printf once >> executed.txt")
			c.wait("completed")
			call := c.call()
			message := "An error occurred; request ID fixture-request-123"
			if kind == "API error" {
				call.failure <- &responsesapi.APIError{Code: "server_error", Message: message}
			} else {
				call.reply <- llm.Response{Failure: &llm.Failure{Code: "server_error", Message: message}}
			}
			c.wait("Provider temporarily unavailable; work stopped.")
			select {
			case <-c.client.calls:
				t.Fatal("chat retried an exhausted/partial generation")
			case <-time.After(100 * time.Millisecond):
			}
			c.send("/status")
			c.wait("Status: idle")
			c.send("continue")
			continued := c.call()
			users := messages(continued.request, llm.RoleUser)
			if len(users) != 2 || users[0] != "do this once" || users[1] != "continue" {
				t.Fatalf("history lost: %v", users)
			}
			continued.reply <- reply("continued after temporary failure")
			c.wait("continued after temporary failure")
			c.finish("/exit")
			data, err := os.ReadFile(filepath.Join(workspace, "executed.txt"))
			if err != nil || string(data) != "once" {
				t.Fatalf("tool repeated: %q %v", data, err)
			}
			paths, _ := filepath.Glob(filepath.Join(workspace, ".harness/sessions/logs/diagnostic-*.jsonl"))
			found := false
			for _, path := range paths {
				for _, record := range records(t, path) {
					if record.Stage == "provider" && record.Event == "request_failed" && strings.Contains(record.Error, "server_error") && strings.Contains(record.Error, "fixture-request-123") && record.Stack != "" {
						found = true
					}
				}
			}
			if !found {
				t.Fatal("provider cause/request ID missing from diagnostics")
			}
		})
	}
}

func TestPermanentProviderFailuresStillSurface(t *testing.T) {
	for _, code := range []string{"invalid_api_key", "insufficient_quota", "bio_policy"} {
		t.Run(code, func(t *testing.T) {
			c := launch(t, t.TempDir(), false)
			c.send("request")
			c.call().failure <- &responsesapi.APIError{StatusCode: 503, Code: code, Message: "requires attention"}
			select {
			case err := <-c.done:
				if err == nil || !strings.Contains(err.Error(), code) {
					t.Fatal("lost permanent cause", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("permanent failure was hidden")
			}
		})
	}
}

func TestProviderFailureDoesNotHideLocalErrors(t *testing.T) {
	temporary := &responsesapi.APIError{Code: "server_error"}
	for _, err := range []error{
		errors.New("server_error"), io.EOF,
		errors.Join(temporary, errors.New("disk full")),
		fmt.Errorf("runtime: %w", errors.Join(temporary, errors.New("output failed"))),
		&diagnosticWriteError{temporary},
	} {
		if recoverableProviderFailure(err) {
			t.Fatalf("hid a local/unknown failure: %v", err)
		}
	}
}

func TestChatProviderAttemptDefaultsAndOverrides(t *testing.T) {
	for _, tt := range []struct {
		args []string
		env  string
		want int
	}{
		{nil, "", 3}, {nil, "2", 2}, {[]string{"-max-attempts", "1"}, "5", 1},
	} {
		c, err := parse(append(tt.args, t.TempDir()), func(name string) string {
			if name == "UNREAL_HARNESS_LLM_MAX_ATTEMPTS" {
				return tt.env
			}
			return ""
		}, io.Discard)
		if err != nil || c.attempts != tt.want {
			t.Fatalf("attempts=%d want=%d error=%v", c.attempts, tt.want, err)
		}
	}
}
