package juice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// KernelError is a typed kernel failure decoded from an error HTTP response
// ({"error": msg, "code": "ErrXxx"}). It preserves the kernel's stable error
// code so callers can branch on it without parsing prose.
type KernelError struct {
	Status  int
	Code    string
	Message string
}

func (e *KernelError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("http %d: %s", e.Status, e.Message)
}

// Code returns the kernel error code of err, or "" if err is not a KernelError.
func Code(err error) string {
	var ke *KernelError
	if errors.As(err, &ke) {
		return ke.Code
	}
	return ""
}

// IsRetryable reports whether err is worth retrying with backoff: transport
// failures and transient kernel conditions (429, genuine 5xx, timeout), but
// never a cancellation, a schema/authz/funds rejection, or a model-output error.
//
// The kernel returns HTTP 500 for execution_failed / schema_violation — a model
// that produced an unusable selection or output. Those are not transient: a
// backoff-retry re-rolls the same prompt for the same likely failure. They are
// excluded here and handled by the loop's feed-back-and-continue instead, which
// gives the model its mistake as context on the next turn.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ke *KernelError
	if errors.As(err, &ke) {
		if ke.Code == "execution_failed" || ke.Code == "schema_violation" {
			return false
		}
		return ke.Status == http.StatusTooManyRequests || ke.Status >= 500 || ke.Code == "timeout"
	}
	// A non-kernel error here is a transport failure (connection refused, reset,
	// DNS) — retryable.
	return true
}
