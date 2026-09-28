package operation

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func approvalManager(t *testing.T, interactive bool) (*LocalOperationManager, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var manager *LocalOperationManager
	if interactive {
		manager = NewLocalOperationManagerWithApprovals(ctx, nil)
	} else {
		manager = NewLocalOperationManager(ctx)
	}
	t.Cleanup(func() {
		cancel()
		for range manager.Updates() {
		}
	})
	return manager, cancel
}

func approvalFixture(t *testing.T, name string) (Operation, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "captures"), 0700); err != nil {
		t.Fatal(err)
	}
	// This is a local fake executable. Tests never invoke SSH or contact a host.
	binary := filepath.Join(dir, name)
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf invoked >> proof\n"), 0700); err != nil {
		t.Fatal(err)
	}
	spec, err := NewShellSpec(ShellInput{Shell: "/bin/sh", Directory: dir, Command: fmt.Sprintf("printf prefix > prefix; %q prod", binary)}, filepath.Join(dir, "captures"), DefaultMaxOutputLength)
	if err != nil {
		t.Fatal(err)
	}
	return Operation{ID: ID(name), Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: StatusReady}, dir
}

func approvalUpdate(t *testing.T, manager *LocalOperationManager) Operation {
	t.Helper()
	select {
	case op, ok := <-manager.Updates():
		if !ok {
			t.Fatal("manager stopped")
		}
		return op
	case <-time.After(5 * time.Second):
		t.Fatal("missing update")
		return Operation{}
	}
}

func approvalTerminal(t *testing.T, manager *LocalOperationManager) Operation {
	t.Helper()
	for {
		op := approvalUpdate(t, manager)
		if localOperationFinished(op.Status) {
			return op
		}
	}
}

func assertNotExecuted(t *testing.T, dir string) {
	t.Helper()
	for _, file := range []string{"proof", "prefix", "captures/ssh", "captures/scp", "captures/rsync"} {
		if _, err := os.Stat(filepath.Join(dir, file)); !os.IsNotExist(err) {
			t.Fatalf("unexpected side effect %s: %v", file, err)
		}
	}
}

func TestShellApprovalOnceAndScoped(t *testing.T) {
	for _, name := range []string{"ssh", "scp", "rsync"} {
		t.Run(name, func(t *testing.T) {
			manager, _ := approvalManager(t, true)
			op, dir := approvalFixture(t, name)
			if err := manager.Add(op); err != nil {
				t.Fatal(err)
			}
			pending := approvalUpdate(t, manager)
			id := ShellApprovalID(pending)
			if id == "" {
				t.Fatal("no permission request")
			}
			assertNotExecuted(t, dir)
			for _, pair := range [][2]string{{string(op.ID), ""}, {string(op.ID), "stale"}, {"other", id}} {
				if err := manager.ResolveShellApproval(ID(pair[0]), pair[1], true); err == nil {
					t.Fatal("invalid approval accepted")
				}
			}
			if err := manager.Add(op); err != nil {
				t.Fatal(err)
			} // no duplicate prompt/run
			if err := manager.ResolveShellApproval(op.ID, id, true); err != nil {
				t.Fatal(err)
			}
			if err := manager.ResolveShellApproval(op.ID, id, true); err == nil {
				t.Fatal("approval reused")
			}
			finished := approvalTerminal(t, manager)
			if finished.Status != StatusCompleted || ShellApprovalID(finished) != "" {
				t.Fatalf("completion: %+v", finished)
			}
			proof, err := os.ReadFile(filepath.Join(dir, "proof"))
			if err != nil || string(proof) != "invoked" {
				t.Fatalf("proof: %q %v", proof, err)
			}
			op.ID += "-second"
			if err := manager.Add(op); err != nil {
				t.Fatal(err)
			}
			if next := approvalUpdate(t, manager); ShellApprovalID(next) == "" || ShellApprovalID(next) == id {
				t.Fatal("permission leaked into next operation")
			}
		})
	}
}

func TestShellApprovalDoesNotBlockOtherOperations(t *testing.T) {
	manager, _ := approvalManager(t, true)
	protected, dir := approvalFixture(t, "ssh")
	if err := manager.Add(protected); err != nil {
		t.Fatal(err)
	}
	pending := approvalUpdate(t, manager)
	spec, err := NewShellSpec(ShellInput{Shell: "/bin/sh", Directory: dir, Command: "printf local-only"}, filepath.Join(dir, "captures"), DefaultMaxOutputLength)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := Operation{ID: "ordinary", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: StatusReady}
	if err := manager.Add(ordinary); err != nil {
		t.Fatal(err)
	}
	finished := approvalTerminal(t, manager)
	if finished.ID != ordinary.ID || finished.Status != StatusCompleted {
		t.Fatalf("ordinary command blocked: %+v", finished)
	}
	assertNotExecuted(t, dir)
	if err := manager.ResolveShellApproval(protected.ID, ShellApprovalID(pending), false); err != nil {
		t.Fatal(err)
	}
	if finished := approvalTerminal(t, manager); finished.ID != protected.ID || finished.Status != StatusFailed {
		t.Fatal("wrong operation denied")
	}
}

func TestShellApprovalDenyCancelAndNoninteractive(t *testing.T) {
	for _, action := range []string{"deny", "cancel", "noninteractive"} {
		t.Run(action, func(t *testing.T) {
			manager, _ := approvalManager(t, action != "noninteractive")
			op, dir := approvalFixture(t, "ssh")
			if err := manager.Add(op); err != nil {
				t.Fatal(err)
			}
			if action != "noninteractive" {
				pending := approvalUpdate(t, manager)
				var err error
				if action == "deny" {
					err = manager.ResolveShellApproval(op.ID, ShellApprovalID(pending), false)
				} else {
					err = manager.Cancel(op.ID, "user canceled")
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := manager.ResolveShellApproval(op.ID, ShellApprovalID(pending), true); err == nil {
					t.Fatal("terminal request authorized")
				}
			}
			finished := approvalTerminal(t, manager)
			if finished.Status == StatusCompleted || ShellApprovalID(finished) != "" {
				t.Fatal("command not rejected")
			}
			if action != "cancel" {
				state, _ := DecodeShellState(finished)
				if !strings.Contains(state.TerminalError, "permission") {
					t.Fatal(state.TerminalError)
				}
			}
			assertNotExecuted(t, dir)
		})
	}
}

func TestShellApprovalRestoreAndPreprocessCheckpoint(t *testing.T) {
	for _, phase := range []ShellPhase{"", ShellPhaseCreateDirectory, ShellPhaseCreateOut, ShellPhaseCreateErr} {
		t.Run(string(phase), func(t *testing.T) {
			manager, cancel := approvalManager(t, true)
			op, dir := approvalFixture(t, "scp")
			if err := manager.Add(op); err != nil {
				t.Fatal(err)
			}
			pending := approvalUpdate(t, manager)
			oldID := ShellApprovalID(pending)
			cancel()
			for range manager.Updates() {
			}
			if phase != "" {
				state, _ := DecodeShellState(pending)
				state.Phase, state.ApprovalID = phase, "" // approved but crashed before process start
				pending.State, _ = json.Marshal(state)
				pending.Status = StatusAwaiting
			}
			next, _ := approvalManager(t, true)
			if err := next.Add(pending); err != nil {
				t.Fatal(err)
			}
			restored := approvalUpdate(t, next)
			if id := ShellApprovalID(restored); id == "" || id == oldID {
				t.Fatal("restored permission not rotated")
			}
			if err := next.ResolveShellApproval(op.ID, oldID, true); err == nil {
				t.Fatal("old permission worked after restart")
			}
			assertNotExecuted(t, dir)
		})
	}
}
