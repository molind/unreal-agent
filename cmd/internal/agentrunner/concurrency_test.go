package agentrunner

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestSQLiteRunnersShareWorkspaceButNotSession(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	args := []string{"-storage-format", "sqlite", "-workspace", workspace, "-session-directory", directory}
	getenv := func(name string) string {
		if name == llmAPIKeyEnvironment {
			return "secret"
		}
		return ""
	}
	entered := make(chan string, 2)
	proceed := make(chan struct{})
	type outcome struct {
		code   int
		output string
	}
	done := make(chan outcome, 2)
	for _, id := range []string{"a", "b"} {
		client := &fakeClient{respond: func(ctx context.Context, request llm.Request) (llm.Response, error) {
			entered <- id
			select {
			case <-proceed:
				return llm.Response{}, nil
			case <-ctx.Done():
				return llm.Response{}, ctx.Err()
			}
		}}
		go func() {
			var output strings.Builder
			code := RunMain(ctx, args, getenv, func() []string { return nil }, strings.NewReader(fmt.Sprintf(`{"session_id":%q,"prompt":"work"}`, id)), io.Discard, &output, testConfig(client))
			done <- outcome{code, output.String()}
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case result := <-done:
			t.Fatalf("runner exited before both sessions were active: %d: %s", result.code, result.output)
		case <-ctx.Done():
			t.Fatal("parallel runners did not reach model", ctx.Err())
		}
	}
	probe, err := localfile.NewSQLite(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if release, err := probe.LockSession("a"); err == nil {
		_ = release()
		t.Fatal("runner did not hold its session")
	}
	// The loser must not reach the model; returning an empty response keeps this
	// test bounded even if the ownership check accidentally regresses.
	called := make(chan struct{}, 1)
	duplicate := &fakeClient{respond: func(context.Context, llm.Request) (llm.Response, error) {
		called <- struct{}{}
		return llm.Response{}, nil
	}}
	var output strings.Builder
	code := RunMain(ctx, args, getenv, func() []string { return nil }, strings.NewReader(`{"session_id":"a","prompt":"duplicate"}`), &output, &output, testConfig(duplicate))
	if code == 0 || !strings.Contains(output.String(), "already in use") {
		t.Fatal("duplicate runner was accepted", code, output.String())
	}
	select {
	case <-called:
		t.Fatal("duplicate runner started model work")
	default:
	}
	close(proceed)
	for range 2 {
		select {
		case result := <-done:
			if result.code != 0 {
				t.Fatal("parallel runner failed", result.code, result.output)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	lease, err := probe.Database().LockWriter()
	if err != nil {
		t.Fatal("runner leaked shared workspace lease", err)
	}
	_ = lease()
}
