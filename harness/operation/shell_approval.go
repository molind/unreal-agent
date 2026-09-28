package operation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/storage"
)

type shellApprovalRequest struct {
	id        ID
	requestID string
	allow     bool
	result    chan error
}

// NewLocalOperationManagerWithApprovals waits for host decisions on ssh/scp/rsync.
// Noninteractive constructors deny these commands instead of hanging or executing.
func NewLocalOperationManagerWithApprovals(ctx context.Context, db *storage.DB) *LocalOperationManager {
	return newLocalOperationManager(ctx, db, true)
}

// ResolveShellApproval authorizes only the exact pending operation/request pair.
// Grants are consumed once and never survive a manager restart.
func (manager *LocalOperationManager) ResolveShellApproval(id ID, requestID string, allow bool) error {
	request := shellApprovalRequest{id: id, requestID: requestID, allow: allow, result: make(chan error, 1)}
	select {
	case manager.approvals <- request:
	case <-manager.ctx.Done():
		return manager.ctx.Err()
	}
	select {
	case err := <-request.result:
		return err
	case <-manager.ctx.Done():
		return manager.ctx.Err()
	}
}

// ShellApprovalID returns only pending permissions, not historical terminal ones.
func ShellApprovalID(op Operation) string {
	if op.Type != TypeShell || localOperationFinished(op.Status) || op.Status == StatusCanceling {
		return ""
	}
	state, err := DecodeShellState(op)
	if err != nil {
		return ""
	}
	return state.ApprovalID
}

// gateShell runs before ANY shell primitive, including when restoring a checkpoint
// from before process start. Interrupted processes are recovered, never re-run.
func (manager *LocalOperationManager) gateShell(current *localRunningOperation) (*Step, error) {
	op := current.operation
	if op.Type != TypeShell || (op.Status != StatusReady && op.Status != StatusAwaiting) {
		return nil, nil
	}
	state, err := DecodeShellState(op)
	if err != nil {
		return nil, err
	}
	switch state.Phase {
	case "", ShellPhaseCreateDirectory, ShellPhaseCreateOut, ShellPhaseCreateErr:
	default:
		return nil, nil
	}
	if !shellNeedsApproval(state.Input.Command) {
		return nil, nil
	}
	if !manager.shellApprovals {
		failed := failLocalOperation(op, errors.New("ssh, scp and rsync require explicit user permission; this host cannot request approval. Command was not executed"))
		return &Step{Operation: &failed}, nil
	}
	// Rotate the token on restore: a stale UI or previously granted request cannot
	// authorize a later run, even when the durable operation ID is unchanged.
	state.ApprovalID = uuid.New().String()
	op.State, err = json.Marshal(state)
	if err != nil {
		return nil, err
	}
	current.approvalID = state.ApprovalID
	return &Step{Operation: &op}, nil
}

func (manager *LocalOperationManager) resolveShellApproval(operations map[ID]*localRunningOperation, request shellApprovalRequest) (int, error) {
	current := operations[request.id]
	if current == nil || request.requestID == "" || current.approvalID != request.requestID {
		return 0, errors.New("shell permission request is no longer pending")
	}
	if err := current.ctx.Err(); err != nil {
		return 0, err
	}
	current.approvalID = "" // Consume before starting anything.
	if !request.allow {
		manager.failLocalOperation(operations, current, errors.New("User denied permission for this shell command. Command was not executed; do not retry without a new user request"))
		return 0, nil
	}
	state, err := DecodeShellState(current.operation)
	if err != nil {
		manager.failLocalOperation(operations, current, err)
		return 0, err
	}
	state.ApprovalID = ""
	current.operation.State, err = json.Marshal(state)
	if err == nil {
		err = current.initialize()
	}
	if err != nil {
		manager.failLocalOperation(operations, current, err)
		return 0, err
	}
	step, err := current.handle(nil)
	if err != nil {
		manager.failLocalOperation(operations, current, err)
		return 0, err
	}
	return manager.acceptLocalStep(operations, current, step), nil
}
