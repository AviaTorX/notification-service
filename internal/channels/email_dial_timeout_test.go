package channels

import (
	"context"
	"net"
	"testing"
	"time"

	"gopkg.in/gomail.v2"

	"github.com/avinash-shinde/notification-service/internal/apperr"
)

// E1: if the SMTP dial stalls, Send must return within e.Timeout — not
// wait for the kernel's TCP timeout. We simulate a stalled dial by
// pointing the adapter at an unroutable address (the RFC 5737 TEST-NET
// range) and asserting the deadline fires.
func TestEmail_DialTimeout_BoundsAttempt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short")
	}
	d := gomail.NewDialer("192.0.2.1", 25, "", "") // RFC 5737 reserved, will time out
	d.TLSConfig = nil
	a := &EmailAdapter{Dialer: d, From: "f@example.com", Timeout: 300 * time.Millisecond}

	start := time.Now()
	_, err := a.Send(context.Background(), Message{
		ToEmail:  "to@example.com",
		Subject:  "s",
		BodyText: "hello",
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a transient error on dial timeout")
	}
	if apperr.KindOf(err) != apperr.KindTransient {
		t.Errorf("err kind=%v, want transient", apperr.KindOf(err))
	}
	// Give ourselves plenty of slack; the important assertion is that we
	// don't wait anywhere close to the kernel's 75s SYN timeout.
	if elapsed > 5*time.Second {
		t.Errorf("dial not bounded by adapter timeout, elapsed=%v", elapsed)
	}
	// Sanity: the error should not outright claim success.
	if _, ok := err.(net.Error); !ok && !apperr.IsRetryable(err) {
		t.Logf("note: non-net.Error classification: %v", err)
	}
}
