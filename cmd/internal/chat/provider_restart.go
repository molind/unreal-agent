package chat

import (
	"github.com/coder/websocket"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
)

// Keeping the UI open grants no retry permission; the transport already decided
// whether resubmission was safe and exhausted its bounded attempts.
func recoverableProviderFailure(err error) bool {
	return recoverableRuntimeCause(err, responsesapi.IsTransientError)
}

// A known provider restart can stop work without terminating the interactive
// app. This grants no retry permission: the transport already exhausted its one
// pre-generation recovery, or observed a partial generation. Preserve unrelated
// storage/log/output errors instead of hiding them behind a recoverable cause.
func recoverableProviderRestart(err error) bool {
	return recoverableRuntimeCause(err, func(cause error) bool {
		switch e := cause.(type) {
		case websocket.CloseError:
			return e.Code == websocket.StatusServiceRestart
		case *websocket.CloseError:
			return e.Code == websocket.StatusServiceRestart
		}
		return false
	})
}
