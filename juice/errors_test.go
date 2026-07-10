package juice

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&KernelError{Status: http.StatusTooManyRequests}, true},
		{&KernelError{Status: 500, Code: "internal"}, true},
		{&KernelError{Status: 503}, true},
		{&KernelError{Code: "timeout", Status: 504}, true},
		// Model-output 500s are NOT retryable — the loop feeds them back instead.
		{&KernelError{Status: 500, Code: "execution_failed"}, false},
		{&KernelError{Status: 422, Code: "schema_violation"}, false},
		{&KernelError{Status: 422, Code: "invalid_input"}, false},
		{&KernelError{Status: 402, Code: "insufficient_funds"}, false},
		{&KernelError{Status: 401, Code: "unauthenticated"}, false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{errors.New("connection refused"), true}, // transport failure
	}
	for i, c := range cases {
		if got := IsRetryable(c.err); got != c.want {
			t.Errorf("case %d (%v): IsRetryable = %v, want %v", i, c.err, got, c.want)
		}
	}
}

func TestCodeExtraction(t *testing.T) {
	if got := Code(&KernelError{Code: "not_found"}); got != "not_found" {
		t.Fatalf("Code = %q", got)
	}
	if got := Code(errors.New("plain")); got != "" {
		t.Fatalf("Code of non-kernel error = %q, want empty", got)
	}
}

func TestKernelErrorMessage(t *testing.T) {
	e := &KernelError{Status: 422, Code: "invalid_input", Message: "bad args"}
	if e.Error() != "invalid_input: bad args" {
		t.Fatalf("Error() = %q", e.Error())
	}
	e2 := &KernelError{Status: 500, Message: "boom"}
	if e2.Error() != "http 500: boom" {
		t.Fatalf("Error() = %q", e2.Error())
	}
}
