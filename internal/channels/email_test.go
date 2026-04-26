package channels

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/avinash-shinde/notification-service/internal/apperr"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifySMTPError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantKind apperr.Kind
	}{
		{"nil", nil, 0},
		{"timeout is transient", timeoutErr{}, apperr.KindTransient},
		{"5xx is permanent", errors.New("550 mailbox unavailable"), apperr.KindPermanent},
		{"everything else is transient", errors.New("connection refused"), apperr.KindTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := classifySMTPError(tc.err)
			if tc.err == nil {
				if out != nil {
					t.Fatalf("want nil, got %v", out)
				}
				return
			}
			if apperr.KindOf(out) != tc.wantKind {
				t.Fatalf("got kind %v want %v", apperr.KindOf(out), tc.wantKind)
			}
		})
	}
}

// Satisfy the net.Error interface assertion path without an extra import.
var _ net.Error = timeoutErr{}
var _ = time.Second
