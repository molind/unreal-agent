package responsesapi

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// Ordinary transient failures honor MaxAttempts. Connection/reference resync is
// still permitted once with MaxAttempts=1, but shares the total budget when it is
// larger. Mixing handshake, API and resync failures cannot multiply retries.
func (a *adapter) retryWebsocketFailure(ctx context.Context, err error, headers http.Header, attempt int, resynchronized *bool) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var failure *websocketFailure
	if !errors.As(err, &failure) || !failure.beforeGeneration || !IsTransientError(err) {
		return false, nil
	}
	if recoverableWebsocketFailure(err) {
		if *resynchronized || attempt >= max(2, a.maxAttempts) {
			return false, nil
		}
		*resynchronized = true
		if websocket.CloseStatus(err) == websocket.StatusServiceRestart {
			if waitErr := waitWebsocketRestart(ctx); waitErr != nil {
				return false, waitErr
			}
		}
		return true, nil
	}
	if attempt >= a.maxAttempts {
		return false, nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) && !failure.beforeSend {
		// A mid-exchange network loss, unlike an explicit API rejection, does
		// not prove whether the request was accepted. Keep the UI usable, but
		// do not automatically resend that generation.
		return false, nil
	}
	policy := a.remoteRequest(nil, "").RetryPolicy
	delay := responseRetryDelay(policy, attempt, apiErr, headers, time.Now(), rand.Float64())
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return ctx.Err() == nil, ctx.Err()
	}
}
