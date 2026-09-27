package responsesapi

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"github.com/coder/websocket"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

// IsTransientError classifies provider availability, NOT permission to replay a
// request. The exchange also checks its generation boundary and attempt budget.
// Hosts may keep their UI open after this error without retrying anything.
// Unknown failures, cancellation and joined independent failures fail closed.
func IsTransientError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	statuses := primitives.DefaultRemoteRequest(remoteSource, "", "").RetryPolicy.RetryableStatusCodes
	for err != nil {
		switch e := err.(type) {
		case *APIError:
			return retryableResponseError(e, statuses)
		case *llm.Failure:
			if e != nil && e.Cause != nil {
				err = e.Cause
				continue
			}
			return e != nil && retryableResponseError(&APIError{Code: e.Code, Message: e.Message}, statuses)
		case *providerTransportError:
			return transientTransportCause(e.error)
		case *websocketFailure:
			return IsTransientError(e.error) || transientTransportCause(e.error)
		case websocket.CloseError:
			return transientWebsocketClose(e.Code)
		case *websocket.CloseError:
			return e != nil && transientWebsocketClose(e.Code)
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) != 1 {
				return false
			}
			err = children[0]
		} else {
			err = errors.Unwrap(err)
		}
	}
	return false
}

// Mark transport errors at the provider boundary, so a local disk/storage timeout
// is not mistaken for provider unavailability merely because it is a net.Error.
type providerTransportError struct{ error }

func (e *providerTransportError) Unwrap() error { return e.error }

func transientTransportCause(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	for err != nil {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) != 1 {
				return false
			}
			err = children[0]
			continue
		}
		if cause := errors.Unwrap(err); cause != nil {
			err = cause
			continue
		}
		switch err {
		case context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF,
			syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.ECONNRESET,
			syscall.EHOSTDOWN, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.ENETUNREACH, syscall.EPIPE, syscall.ETIMEDOUT:
			return true
		}
		if network, ok := err.(net.Error); ok && network.Timeout() {
			return true
		}
		if dns, ok := err.(*net.DNSError); ok && dns.IsTemporary {
			return true
		}
		return false
	}
	return false
}

func transientWebsocketClose(code websocket.StatusCode) bool {
	switch code {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusAbnormalClosure,
		websocket.StatusInternalError, websocket.StatusServiceRestart, websocket.StatusTryAgainLater:
		return true
	}
	return false
}
