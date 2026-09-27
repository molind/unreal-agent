package responsesapi

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

var retryAfterMessagePattern = regexp.MustCompile(`(?i)\btry again in\s*(\d+(?:\.\d+)?)\s*(ms|milliseconds?|s|seconds?)\b`)

// Classify explicit temporary failures, never arbitrary in-band errors. Permanent
// codes/types override retryable statuses; an unknown 2xx error is not permission
// to retry. HTTP and WebSocket exchanges, and the chat UI, share these rules.
func retryableResponseError(err *APIError, retryableStatuses []int) bool {
	if err == nil {
		return false
	}
	switch err.Code {
	case "context_length_exceeded", "insufficient_quota", "usage_not_included", "usage_limit_reached",
		"credit_balance_exhausted", "billing_hard_limit_reached",
		"cyber_policy", "misalignment_policy_violation", "invalid_prompt", "bio_policy",
		"invalid_api_key", "invalid_token", "authentication_error", "permission_denied", "access_denied",
		"policy_violation", "content_policy_violation", "account_deactivated":
		return false
	}
	switch err.Type {
	case "authentication_error", "permission_error", "insufficient_quota", "billing_error", "policy_violation":
		return false
	}
	// Connection-scoped references can expire with an HTTP-style 400 even
	// though a full resynchronization is valid. Auth/policy types above win.
	if err.StatusCode == http.StatusBadRequest && (err.Code == "previous_response_not_found" || err.Code == "websocket_connection_limit_reached") {
		return true
	}
	if err.StatusCode != 0 && (err.StatusCode < http.StatusOK || err.StatusCode >= http.StatusMultipleChoices) {
		return slices.Contains(retryableStatuses, err.StatusCode)
	}
	switch err.Code {
	case "server_error", "internal_error", "service_unavailable", "temporarily_unavailable",
		"server_is_overloaded", "overloaded", "overloaded_error", "slow_down",
		"rate_limit_exceeded", "rate_limit_error", "too_many_requests", "timeout", "request_timeout",
		"previous_response_not_found", "websocket_connection_limit_reached":
		return true
	}
	if status, parseErr := strconv.Atoi(err.Code); parseErr == nil {
		return slices.Contains(retryableStatuses, status)
	}
	if err.Code == "" && (err.Type == "server_error" || err.Type == "overloaded_error" || err.Type == "rate_limit_error") {
		return true
	}
	// Fireworks sometimes labels rate limiting as an invalid request in a
	// successful stream. Do not extend that exception to arbitrary error text.
	return err.Code == "invalid_request_error" && strings.Contains(strings.ToLower(err.Message), "rate limit exceeded")
}

func responseRetryDelay(policy primitives.RemoteRetryPolicy, attempt int, err *APIError, headers http.Header, now time.Time, jitter float64) time.Duration {
	hint := retryAfterHeader(headers.Get("Retry-After"), now)
	if err != nil && err.Code == "rate_limit_exceeded" {
		hint = max(hint, retryAfterMessage(err.Message))
	}
	if hint > 0 {
		return min(hint, policy.MaxBackoff)
	}
	if err != nil && (err.Code == "server_is_overloaded" || err.Code == "slow_down") {
		// Unlike the Codex CLI, unattended runs retry overloads with a longer backoff.
		policy.InitialBackoff = 10 * time.Second
		policy.MaxBackoff = time.Minute
	}
	delay := policy.Backoff(attempt)
	return delay - time.Duration(float64(delay/5)*jitter)
}

func retryAfterHeader(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds > uint64((1<<63-1)/time.Second) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		return max(0, deadline.Sub(now))
	}
	return 0
}

func retryAfterMessage(message string) time.Duration {
	match := retryAfterMessagePattern.FindStringSubmatch(message)
	if match == nil {
		return 0
	}
	unit := "s"
	if strings.HasPrefix(strings.ToLower(match[2]), "m") {
		unit = "ms"
	}
	delay, err := time.ParseDuration(match[1] + unit)
	if err != nil {
		return 0
	}
	return delay
}
