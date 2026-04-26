// Package e2e runs end-to-end tests against the running docker-compose stack.
// It assumes `docker compose up -d` has already brought up api + worker +
// mailhog + slack-mock. Set E2E=1 to enable; otherwise the suite is skipped.
//
// Keeping this out of the default `go test ./...` run is deliberate:
//   - Unit tests must run on a bare checkout with no infra.
//   - E2E is opt-in (`E2E=1 go test ./tests/e2e/...`) so reviewers choose
//     whether to pay the docker-compose cost.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	demoUser    = "00000000-0000-0000-0000-000000000001"
	defaultAPIK = "dev-api-key-change-me"
)

// Hostnames default to host-side ports from docker-compose. Override via env
// for in-network runs (e.g. from a sidecar container inside the compose net).
func apiBase() string     { return envOr("E2E_API_BASE", "http://localhost:8080") }
func mailpitBase() string { return envOr("E2E_MAILPIT_BASE", "http://localhost:8025") }

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func apiKey() string {
	if k := os.Getenv("API_KEY"); k != "" {
		return k
	}
	return defaultAPIK
}

func gate(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to run end-to-end tests against the running stack")
	}
}

func post(t *testing.T, path string, body any, headers map[string]string) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, apiBase()+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, apiBase()+path, nil)
	req.Header.Set("X-API-Key", apiKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestE2E_InstantEmailAndInApp(t *testing.T) {
	gate(t)

	clearMailpit(t)
	req := map[string]any{
		"user_id":       demoUser,
		"template_name": "welcome",
		"variables":     map[string]any{"name": "E2E", "product": "Testing"},
	}
	code, body := post(t, "/v1/notifications", req, nil)
	if code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", code, string(body))
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &created)

	// Wait for worker to flush.
	waitForNotificationStatus(t, created.ID, "sent", 10*time.Second)

	// Email landed in Mailpit.
	msgs := mailpitMessages(t)
	if !strings.Contains(msgs, "Welcome to Testing, E2E") {
		t.Errorf("mailpit body missing expected subject:\n%s", msgs)
	}

	// In-app persisted. Response shape is {"items": [...], "next_cursor"?}
	// since G2 added keyset pagination.
	_, inapp := get(t, "/v1/users/"+demoUser+"/in-app-notifications?limit=5")
	if !strings.Contains(string(inapp), "\"items\"") {
		t.Errorf("in-app response missing items wrapper: %s", string(inapp))
	}
	if !strings.Contains(string(inapp), "Welcome to Testing, E2E") {
		t.Errorf("in-app missing expected title: %s", string(inapp))
	}
}

func TestE2E_ScheduledDelivery(t *testing.T) {
	gate(t)

	at := time.Now().UTC().Add(3 * time.Second).Format(time.RFC3339)
	req := map[string]any{
		"user_id":        demoUser,
		"template_name":  "welcome",
		"variables":      map[string]any{"name": "Later", "product": "Testing"},
		"scheduled_for":  at,
	}
	code, body := post(t, "/v1/notifications", req, nil)
	if code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", code, string(body))
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &created)

	// Should NOT be sent immediately.
	time.Sleep(1 * time.Second)
	_, early := get(t, "/v1/notifications/"+created.ID)
	if strings.Contains(string(early), `"status": "sent"`) || strings.Contains(string(early), `"status":"sent"`) {
		t.Fatalf("scheduled notification sent early: %s", string(early))
	}
	waitForNotificationStatus(t, created.ID, "sent", 10*time.Second)
}

func TestE2E_IdempotencyReplayAndConflict(t *testing.T) {
	gate(t)

	key := fmt.Sprintf("e2e-idem-%d", time.Now().UnixNano())
	req := map[string]any{
		"user_id":       demoUser,
		"template_name": "welcome",
		"variables":     map[string]any{"name": "Idem", "product": "Testing"},
	}

	code1, body1 := post(t, "/v1/notifications", req, map[string]string{"Idempotency-Key": key})
	if code1 != http.StatusAccepted {
		t.Fatalf("first request status=%d", code1)
	}
	var first struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body1, &first)

	code2, body2 := post(t, "/v1/notifications", req, map[string]string{"Idempotency-Key": key})
	if code2 != http.StatusAccepted {
		t.Fatalf("replay status=%d body=%s", code2, string(body2))
	}
	var second struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body2, &second)
	if first.ID != second.ID {
		t.Errorf("idempotent replay returned new id: %s vs %s", first.ID, second.ID)
	}

	// Different body, same key => conflict.
	req2 := map[string]any{
		"user_id":       demoUser,
		"template_name": "welcome",
		"variables":     map[string]any{"name": "Different", "product": "Testing"},
	}
	codeC, _ := post(t, "/v1/notifications", req2, map[string]string{"Idempotency-Key": key})
	if codeC != http.StatusConflict {
		t.Errorf("expected 409, got %d", codeC)
	}
}

func TestE2E_MissingVariablesRejected(t *testing.T) {
	gate(t)
	req := map[string]any{
		"user_id":       demoUser,
		"template_name": "welcome",
		"variables":     map[string]any{"name": "OnlyName"}, // missing product
	}
	code, body := post(t, "/v1/notifications", req, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", code, string(body))
	}
	if !strings.Contains(string(body), "product") {
		t.Errorf("error should name missing variable: %s", string(body))
	}
}

// ---------- helpers ----------

func waitForNotificationStatus(t *testing.T, id, status string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		_, body := get(t, "/v1/notifications/"+id)
		if strings.Contains(string(body), `"status":"`+status+`"`) || strings.Contains(string(body), `"status": "`+status+`"`) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	_, last := get(t, "/v1/notifications/"+id)
	t.Fatalf("timeout waiting for notification %s to reach %s: last=%s", id, status, string(last))
}

func clearMailpit(t *testing.T) {
	req, _ := http.NewRequest(http.MethodDelete, mailpitBase()+"/api/v1/messages", nil)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func mailpitMessages(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(mailpitBase() + "/api/v1/messages")
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(body), "Welcome") {
			return string(body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}
