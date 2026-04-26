package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/avinash-shinde/notification-service/internal/apperr"
)

// SlackAdapter posts to a Slack-compatible incoming webhook. In dev this
// hits the local slack-mock container. In production, swap the URL and
// optionally point at block-kit JSON instead of the text payload.
type SlackAdapter struct {
	HTTPClient *http.Client
	Fallback   string // used when the user has no per-user slack_webhook
}

// NewSlackAdapter builds an adapter with an HTTP transport tuned for
// high-volume webhook traffic:
//   - Connection reuse across sends via a generous MaxIdleConnsPerHost
//     — all writes go to a single Slack hook hostname.
//   - Separate dial + TLS + response-header timeouts so a slow provider
//     can't stall a connection for the full 10s overall budget.
//   - HTTP/2 auto-negotiation (default) so a single conn can carry many
//     concurrent posts.
func NewSlackAdapter(fallbackURL string) *SlackAdapter {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100, // Slack posts go to one host
		MaxConnsPerHost:       0,   // unlimited; the remote is rate-limited, not us
		ForceAttemptHTTP2:     true,
	}
	return &SlackAdapter{
		// 10s overall client timeout matches the worker's per-task
		// latency budget; any webhook that takes longer than that is
		// already going to miss the service's p99 SLO — retry is
		// cheaper than waiting.
		HTTPClient: &http.Client{Transport: transport, Timeout: 10 * time.Second},
		Fallback:   fallbackURL,
	}
}

func (s *SlackAdapter) Name() string { return "slack" }

type slackPayload struct {
	Text           string `json:"text"`
	NotificationID string `json:"notification_id,omitempty"`
}

func (s *SlackAdapter) Send(ctx context.Context, msg Message) (Result, error) {
	url := msg.ToSlackURL
	if url == "" {
		url = s.Fallback
	}
	if url == "" {
		return Result{}, apperr.Permanent("slack webhook missing", ErrMissingDestination)
	}
	body, err := json.Marshal(slackPayload{Text: msg.BodyText, NotificationID: msg.NotificationID})
	if err != nil {
		return Result{}, apperr.Permanent("marshal slack payload", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Result{}, apperr.Permanent("build slack request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return Result{}, apperr.Transient("slack post failed", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Result{ProviderID: fmt.Sprintf("slack:%d", resp.StatusCode)}, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return Result{}, apperr.Transient(
			fmt.Sprintf("slack %d: %s", resp.StatusCode, string(respBody)),
			fmt.Errorf("status %d", resp.StatusCode))
	default:
		return Result{}, apperr.Permanent(
			fmt.Sprintf("slack %d: %s", resp.StatusCode, string(respBody)),
			fmt.Errorf("status %d", resp.StatusCode))
	}
}
