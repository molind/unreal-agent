package responsesapi

import (
	"context"
	"crypto/x509"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/coder/websocket"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

func TestTransientProviderErrorClassification(t *testing.T) {
	for _, err := range []error{
		&APIError{Code: "server_error"}, &APIError{StatusCode: 200, Code: "internal_error"},
		&APIError{Code: "service_unavailable"}, &APIError{Code: "server_is_overloaded"},
		&APIError{Code: "rate_limit_exceeded"}, &APIError{StatusCode: 503}, &APIError{StatusCode: 429},
		&APIError{Code: "timeout"}, &llm.Failure{Code: "server_error"},
		websocket.CloseError{Code: websocket.StatusServiceRestart},
		&websocketFailure{error: websocket.CloseError{Code: websocket.StatusInternalError}},
		&providerTransportError{io.ErrUnexpectedEOF},
		&providerTransportError{&net.OpError{Op: "read", Err: syscall.ECONNRESET}},
		&providerTransportError{context.DeadlineExceeded},
	} {
		if !IsTransientError(fmt.Errorf("provider: %w", errors.Join(err))) {
			t.Errorf("lost transient provider error: %v", err)
		}
	}
	for _, err := range []error{
		nil, context.Canceled, io.EOF, syscall.ETIMEDOUT, errors.New("server_error"),
		&APIError{StatusCode: 200, Code: "unknown"}, &APIError{StatusCode: 401, Code: "server_error"},
		&APIError{StatusCode: 503, Code: "insufficient_quota"}, &APIError{StatusCode: 429, Code: "invalid_api_key"},
		&APIError{StatusCode: 503, Code: "context_length_exceeded"},
		&APIError{StatusCode: 503, Type: "authentication_error", Code: "server_error"},
		&APIError{StatusCode: 503, Type: "permission_error"}, &APIError{StatusCode: 503, Code: "bio_policy"},
		&APIError{StatusCode: 429, Code: "credit_balance_exhausted"}, &llm.Failure{Code: "invalid_token"},
		&providerTransportError{context.Canceled}, &providerTransportError{x509.UnknownAuthorityError{}},
		&providerTransportError{errors.New("local configuration failure")},
		websocket.CloseError{Code: websocket.StatusPolicyViolation},
		errors.Join(&APIError{Code: "server_error"}, errors.New("disk full")),
		&providerTransportError{fmt.Errorf("transport: %w", errors.Join(syscall.ECONNRESET, errors.New("unrelated failure")))},
	} {
		if IsTransientError(err) {
			t.Errorf("misclassified permanent/local/mixed error: %v", err)
		}
	}
}

func TestRemoteFailureRetainsTypedCauseWithoutSerializingIt(t *testing.T) {
	cause := fmt.Errorf("read: %w", syscall.ECONNRESET)
	event := primitives.PrimitiveEvent{Type: primitives.PrimitiveEventFailed, Result: primitives.PrimitiveFailureResult{Error: "read failed", Cause: cause}}
	err := remoteFailureError(context.Background(), event)
	if !errors.Is(err, syscall.ECONNRESET) || !IsTransientError(err) {
		t.Fatalf("lost transport identity: %v", err)
	}
	data, err := json.Marshal(event.Result)
	if err != nil || string(data) != `{"Error":"read failed"}` {
		t.Fatalf("cause leaked into serialized event: %s %v", data, err)
	}
}

func TestFailedHTTPResponseRetainsPermanentClassification(t *testing.T) {
	for _, kind := range []string{"authentication_error", "permission_error", "insufficient_quota", "policy_violation"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"failure\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"type\":%q,\"message\":\"requires attention\"},\"output\":[]}}\n\n", kind)
			}))
			defer server.Close()
			a := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: new(3)})
			response, err := a.Respond(t.Context(), validRequest(), llm.RequestOptions{})
			if err != nil || response.Failure == nil || IsTransientError(response.Failure) || calls.Load() != 1 {
				t.Fatalf("permanent type lost: calls=%d response=%+v err=%v", calls.Load(), response, err)
			}
			var original *APIError
			if !errors.As(response.Failure, &original) || original.Type != kind {
				t.Fatal("typed failure cause lost")
			}
			data, err := json.Marshal(response.Failure)
			if err != nil || string(data) != `{"Code":"server_error","Message":"requires attention"}` {
				t.Fatalf("canonical failed-response encoding changed: %s %v", data, err)
			}
		})
	}
}
