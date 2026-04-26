package channels

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"gopkg.in/gomail.v2"

	"github.com/avinash-shinde/notification-service/internal/apperr"
)

// defaultSMTPTimeout bounds a single dial+send attempt so a cancelled
// context cannot leave a zombie goroutine hanging on a stalled TCP
// connection until the kernel's default SYN timeout.
const defaultSMTPTimeout = 15 * time.Second

type EmailAdapter struct {
	Dialer  *gomail.Dialer
	From    string
	Timeout time.Duration // hard cap for one Dial+Send attempt
}

func NewEmailAdapter(host string, port int, user, pass, from string) *EmailAdapter {
	d := gomail.NewDialer(host, port, user, pass)
	// Dev default: MailHog/Mailpit are plaintext. Enable StartTLS only if a
	// real provider is configured. Safe because production should inject a
	// proper dialer via DI.
	d.TLSConfig = nil
	return &EmailAdapter{Dialer: d, From: from, Timeout: defaultSMTPTimeout}
}

func (e *EmailAdapter) Name() string { return "email" }

// Send delivers the email via the configured SMTP dialer.
//
// Cancellation & timeout (combined fix for reviewer issues G4, E1, F1):
//
//  1. A single `sendCtx` bounds the whole dial + SMTP conversation.
//  2. The dial runs in a goroutine whose result is raced against
//     `sendCtx.Done()` in a select. One dial, no preflight — an earlier
//     revision did a preflight `net.DialTimeout` followed by the real
//     gomail.Dial() to "fail fast on unreachable hosts", which was a
//     double connect and a lying abstraction (it proved nothing about
//     the second dial). Removed.
//  3. If `sendCtx` fires while the dial is in flight, we walk away and
//     leave a small background goroutine to Close() any late-arriving
//     SendCloser so we don't leak file descriptors.
//  4. If `sendCtx` fires during the SMTP conversation, Close() on the
//     SendCloser tears down the TCP connection; the in-flight Write
//     returns with "use of closed network connection" and the goroutine
//     exits within a socket-close, not a TCP timeout.
//
// Known limitation: gomail.v2 does not expose a dial-timeout knob. When
// the host is unreachable, the *underlying* net.Dial can still take up
// to the OS TCP SYN timeout (~75s on Linux) before the goroutine returns.
// We accept that as best-effort: the *caller* is never blocked past
// `sendCtx`, only the background goroutine keeps running until the
// kernel gives up. For true bounded dials, the migration path is
// wneessen/go-mail — documented in DESIGN.md §14.
func (e *EmailAdapter) Send(ctx context.Context, msg Message) (Result, error) {
	if msg.ToEmail == "" {
		return Result{}, apperr.Permanent("email recipient missing", ErrMissingDestination)
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultSMTPTimeout
	}
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	m := gomail.NewMessage()
	m.SetHeader("From", e.From)
	m.SetHeader("To", msg.ToEmail)
	m.SetHeader("Subject", msg.Subject)
	if msg.NotificationID != "" {
		m.SetHeader("X-Notification-ID", msg.NotificationID)
	}
	if msg.BodyText != "" {
		m.SetBody("text/plain", msg.BodyText)
	}
	if msg.BodyHTML != "" {
		if msg.BodyText != "" {
			m.AddAlternative("text/html", msg.BodyHTML)
		} else {
			m.SetBody("text/html", msg.BodyHTML)
		}
	}

	type dialed struct {
		sc  gomail.SendCloser
		err error
	}
	dialCh := make(chan dialed, 1)
	go func() {
		sc, err := e.Dialer.Dial()
		dialCh <- dialed{sc: sc, err: err}
	}()

	var sc gomail.SendCloser
	select {
	case <-sendCtx.Done():
		// Dial is still in flight on the unreachable-host path. Drain the
		// goroutine asynchronously so the fd and the SendCloser are
		// released once the OS gives up on the connect.
		go func() {
			if d := <-dialCh; d.sc != nil {
				_ = d.sc.Close()
			}
		}()
		return Result{}, apperr.Transient("email dial cancelled", sendCtx.Err())
	case d := <-dialCh:
		if d.err != nil {
			return Result{}, classifySMTPError(d.err)
		}
		sc = d.sc
	}

	done := make(chan error, 1)
	go func() {
		done <- gomail.Send(sc, m)
		_ = sc.Close()
	}()

	select {
	case <-sendCtx.Done():
		_ = sc.Close()
		<-done
		return Result{}, apperr.Transient("email send cancelled", sendCtx.Err())
	case err := <-done:
		if err != nil {
			return Result{}, classifySMTPError(err)
		}
	}
	return Result{ProviderID: providerIDHost(e.Dialer)}, nil
}

// classifySMTPError tags well-known SMTP failure modes as transient or
// permanent. Gomail/net/smtp don't expose typed errors, so we fall back to
// net.Error.Timeout() + a small string check.
func classifySMTPError(err error) error {
	if err == nil {
		return nil
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return apperr.Transient("smtp timeout", err)
	}
	msg := err.Error()
	// 5xx range in SMTP is permanent (bad recipient, message rejected).
	// Any other failure we treat as transient to let Asynq retry.
	for _, code := range []string{"550 ", "551 ", "552 ", "553 ", "554 "} {
		if strings.Contains(msg, code) {
			return apperr.Permanent("smtp rejected: "+msg, err)
		}
	}
	return apperr.Transient("smtp send failed", err)
}

func providerIDHost(d *gomail.Dialer) string {
	return fmt.Sprintf("smtp:%s:%s", d.Host, strconv.Itoa(d.Port))
}
