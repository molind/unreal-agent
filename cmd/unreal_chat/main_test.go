package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Exercise the real executable, stdin polling, provider selection, Responses
// adapter, local shell tools, session persistence, Ctrl-C, and process exit.
// The only endpoint is an in-process loopback server; credentials are fake.
func TestScriptedCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "unreal_chat")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	var calls atomic.Int32
	requests := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		index := calls.Add(1)
		requests <- string(body)
		var output any
		switch index {
		case 1:
			output = []any{map[string]any{"type": "function_call", "id": "fc-1", "call_id": "local-tool", "name": "Bash", "arguments": `{"command":"printf local-test > cli-proof.txt"}`}}
		case 2:
			output = []any{map[string]any{"type": "message", "id": "msg-1", "role": "assistant", "status": "completed", "content": []any{map[string]string{"type": "output_text", "text": "Local tool done."}}}}
		case 3:
			<-r.Context().Done()
			return
		default:
			output = []any{map[string]any{"type": "message", "id": "msg-2", "role": "assistant", "status": "completed", "content": []any{map[string]string{"type": "output_text", "text": "After interrupt."}}}}
		}
		response, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp-%d", index), "status": "completed", "output": output}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", response)
	}))
	defer server.Close()
	workspace := t.TempDir()
	cmd := exec.CommandContext(ctx, binary, "-provider", "openai", "-base-url", server.URL, "-model", "local-test", workspace)
	// Do not inherit provider credentials or proxy configuration from the host.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "OPENAI_API_KEY=local-fake-key", "XDG_STATE_HOME=" + filepath.Join(workspace, "state")}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = cmd.Process.Kill() }()
	lines := make(chan string, 128)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	wait := func(want string) {
		t.Helper()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("CLI closed waiting for %q", want)
				}
				if strings.ContainsRune(line, 27) {
					t.Fatal("escape sequences in piped output")
				}
				if strings.Contains(line, want) {
					return
				}
			case <-ctx.Done():
				t.Fatalf("waiting for %q: %v", want, ctx.Err())
			}
		}
	}
	send := func(text string) {
		t.Helper()
		if _, err := fmt.Fprintln(input, text); err != nil {
			t.Fatal(err)
		}
	}
	wait("Type /help")
	send("implement local proof")
	wait("assistant> Local tool done.")
	if body, err := os.ReadFile(filepath.Join(workspace, "cli-proof.txt")); err != nil || string(body) != "local-test" {
		t.Fatalf("tool proof: %s, %v", body, err)
	}
	send("generate slowly")
	for calls.Load() < 3 {
		select {
		case <-requests:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	wait("Stopped.")
	send("next message")
	wait("assistant> After interrupt.")
	send("/exit")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatalf("model calls = %d; stop must not retry", calls.Load())
	}
	// Drained requests must contain old input once, use the explicit provider,
	// and expose the selected effort to the HTTP adapter.
	close(requests)
	var last string
	for body := range requests {
		last = body
	}
	if strings.Count(last, "implement local proof") != 1 || !strings.Contains(last, `"effort":"xhigh"`) || !strings.Contains(last, `"model":"local-test"`) {
		t.Fatal(last)
	}
}

func TestCLIBrokenPipeCleansShell(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "unreal_chat")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		response, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "tool-response", "status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "sleeper", "name": "Bash", "arguments": `{"command":"echo $$ > shell.pid; exec sleep 60"}`}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", response)
	}))
	defer server.Close()
	workspace := t.TempDir()
	cmd := exec.CommandContext(ctx, binary, "-provider", "openai", "-base-url", server.URL, workspace)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "OPENAI_API_KEY=fake-key", "XDG_STATE_HOME=" + filepath.Join(workspace, "state")}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = cmd.Process.Kill() }()
	if _, err := fmt.Fprintln(input, "run shell"); err != nil {
		t.Fatal(err)
	}
	var pid int
	for pid == 0 {
		data, err := os.ReadFile(filepath.Join(workspace, "shell.pid"))
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		select {
		case <-ctx.Done():
			t.Fatal("shell did not start")
		case <-time.After(time.Millisecond):
		}
	}
	defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }()
	// Closing stdout must produce EPIPE, not SIGPIPE termination that bypasses
	// cleanup. Keep stdin open: exit must also join its blocked reader.
	_ = output.Close()
	_, _ = fmt.Fprintln(input, "/status")
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v; stderr=%s", err, &stderr)
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("shell process group %d survived output failure: %v", pid, err)
	}
}
