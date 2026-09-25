// Package coordinator defines the owner of the central event loop.
package coordinator

import (
	"context"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type Dependencies struct {
	// Opt in to error-driven context recovery. No model limit is assumed.
	RecoverContext bool
	// Called on the coordinator goroutine; observers must not reenter it.
	OnContextChange func(contextbuilder.Status)
	// Opt in to replaying full canonical history instead of a stale saved
	// compaction. The invalid summary is never applied. Callback failures remain
	// fatal; hosts should make this recovery visible to the user.
	RecoverStaleCompactions bool
	OnCompactionSkipped     func(session.TurnID, error) error

	// OnIdleChange observes settled scheduling state, initially and on changes.
	// Idle means no pending input, model request, or tool work. Called on the
	// Run goroutine; the observer must not reenter the coordinator.
	OnIdleChange func(idle bool)

	// JoinModels waits for canceled adapter calls before Run returns.
	// Adapters must honor cancellation. Useful for runtime switching.
	JoinModels            bool
	ToolHeartbeatInterval time.Duration
	SessionID             session.ID
	Inbox                 *inbox.Inbox
	Restored              sessionstore.ResumeState
	Sessions              sessionstore.Store
	ContextBuilder        contextbuilder.Builder
	LLM                   llm.Adapter
	Tools                 tool.Registry
	Operations            operation.Manager
}

type Coordinator interface {
	// Run owns one session's decision loop until a stop control completes or
	// the context is canceled. It returns nil for a completed stop. It is
	// single-use; its caller must cancel the Inbox when Run returns.
	Run(context.Context) error
}

func New(dependencies Dependencies) Coordinator {
	return &coordinator{
		dependencies: dependencies,
		state:        newLoopState(),
	}
}
