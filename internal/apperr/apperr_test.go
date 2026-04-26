package apperr

import (
	"errors"
	"testing"
)

func TestKindAndRetryable(t *testing.T) {
	cases := []struct {
		err       error
		kind      Kind
		retryable bool
	}{
		{Invalid("nope"), KindInvalid, false},
		{NotFound("x"), KindNotFound, false},
		{Conflict("x"), KindConflict, true},
		{RateLimited("x"), KindRateLimited, true},
		{Transient("boom", errors.New("net")), KindTransient, true},
		{Permanent("no", errors.New("bad")), KindPermanent, false},
		{errors.New("plain"), KindInternal, true},
	}
	for _, tc := range cases {
		if got := KindOf(tc.err); got != tc.kind {
			t.Errorf("KindOf(%v) = %v, want %v", tc.err, got, tc.kind)
		}
		if got := IsRetryable(tc.err); got != tc.retryable {
			t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.retryable)
		}
	}
}

func TestErrorMessage_WrapsCause(t *testing.T) {
	base := errors.New("root cause")
	err := Transient("dial failed", base)
	if !errors.Is(err, base) {
		t.Error("errors.Is should unwrap to the base error")
	}
	if err.Error() != "dial failed: root cause" {
		t.Errorf("got %q", err.Error())
	}
}
