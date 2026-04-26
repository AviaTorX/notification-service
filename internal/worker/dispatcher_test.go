package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	dto "github.com/prometheus/client_model/go"

	"github.com/avinash-shinde/notification-service/internal/apperr"
	"github.com/avinash-shinde/notification-service/internal/channels"
	"github.com/avinash-shinde/notification-service/internal/observability"
	"github.com/avinash-shinde/notification-service/internal/queue"
	"github.com/avinash-shinde/notification-service/internal/store"
)

// --- in-memory fakes ---

type fakeStore struct {
	notif           *store.Notification
	user            *store.User
	deliveries      map[string]*store.Delivery // keyed by channel
	prefs           []store.ChannelPreference
	listErr         error
	notFoundOn      map[string]bool // "user" / "notif"
	updateStatusErr error           // if set, UpdateNotificationStatus returns this
}

func newFakeStore(n *store.Notification, u *store.User) *fakeStore {
	return &fakeStore{
		notif:      n,
		user:       u,
		deliveries: map[string]*store.Delivery{},
		notFoundOn: map[string]bool{},
	}
}

func (f *fakeStore) GetNotification(_ context.Context, id uuid.UUID) (*store.Notification, error) {
	if f.notFoundOn["notif"] {
		return nil, store.ErrNotFound
	}
	if f.notif == nil || f.notif.ID != id {
		return nil, store.ErrNotFound
	}
	copy := *f.notif
	return &copy, nil
}

func (f *fakeStore) GetUser(_ context.Context, _ uuid.UUID) (*store.User, error) {
	if f.notFoundOn["user"] {
		return nil, store.ErrNotFound
	}
	return f.user, nil
}

func (f *fakeStore) EnabledChannels(_ context.Context, _ uuid.UUID, candidate []string) ([]string, error) {
	disabled := map[string]bool{}
	for _, p := range f.prefs {
		if !p.Enabled {
			disabled[p.Channel] = true
		}
	}
	out := make([]string, 0, len(candidate))
	for _, c := range candidate {
		if !disabled[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateNotificationStatus(_ context.Context, _ uuid.UUID, status string, _ *string) error {
	if f.updateStatusErr != nil {
		return f.updateStatusErr
	}
	if f.notif != nil {
		f.notif.Status = status
	}
	return nil
}

func (f *fakeStore) EnsureDelivery(_ context.Context, nid uuid.UUID, channel string) (*store.Delivery, error) {
	d, ok := f.deliveries[channel]
	if !ok {
		d = &store.Delivery{NotificationID: nid, Channel: channel, Status: "pending"}
		f.deliveries[channel] = d
	}
	return d, nil
}

func (f *fakeStore) UpdateDelivery(_ context.Context, _ uuid.UUID, channel, status string, providerID, errMsg *string) error {
	d := f.deliveries[channel]
	if d == nil {
		d = &store.Delivery{Channel: channel}
		f.deliveries[channel] = d
	}
	d.Status = status
	d.Attempts++
	if providerID != nil {
		d.ProviderID = providerID
	}
	if errMsg != nil {
		copy := *errMsg
		d.Error = &copy
	}
	return nil
}

func (f *fakeStore) ListDeliveries(_ context.Context, _ uuid.UUID) ([]store.Delivery, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.Delivery, 0, len(f.deliveries))
	for _, d := range f.deliveries {
		out = append(out, *d)
	}
	return out, nil
}

// --- scripted channel adapter ---

type scriptedAdapter struct {
	name    string
	outcome error // nil = success
	calls   int
	lastMsg channels.Message
}

func (s *scriptedAdapter) Name() string { return s.name }
func (s *scriptedAdapter) Send(_ context.Context, msg channels.Message) (channels.Result, error) {
	s.calls++
	s.lastMsg = msg
	if s.outcome != nil {
		return channels.Result{}, s.outcome
	}
	return channels.Result{ProviderID: s.name + ":ok"}, nil
}

type registryStub struct {
	m map[string]channels.Adapter
}

func (r *registryStub) Get(name string) (channels.Adapter, error) {
	a, ok := r.m[name]
	if !ok {
		return nil, errors.New("unknown channel " + name)
	}
	return a, nil
}

// --- helpers ---

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newTask(t *testing.T, nid uuid.UUID) *asynq.Task {
	t.Helper()
	body, err := json.Marshal(queue.SendPayload{NotificationID: nid})
	must(t, err)
	return asynq.NewTask(queue.TaskSendNotification, body)
}

func seedNotification() *store.Notification {
	subject := "hi"
	body := "hello"
	name := "test_tmpl"
	return &store.Notification{
		ID:                   uuid.New(),
		UserID:               uuid.New(),
		Channels:             []string{"email", "slack"},
		Variables:            map[string]any{},
		Status:               "queued",
		SubjectSnapshot:      &subject,
		BodyTextSnapshot:     &body,
		TemplateNameSnapshot: &name,
	}
}

func seedUser() *store.User {
	e := "x@example.com"
	w := "http://slack-mock/w"
	return &store.User{Email: &e, SlackWebhook: &w}
}

// --- tests ---

// The B1 bug: one success + one transient failure used to end up at
// partially_sent and never retried. The fix requires the handler to return
// a retry-triggering error (not nil + not SkipRetry) when any transient
// failure remains.
func TestDispatcher_PartialTransientFailure_ReturnsRetryableError(t *testing.T) {
	n := seedNotification()
	fs := newFakeStore(n, seedUser())
	emailA := &scriptedAdapter{name: "email"}                                               // ok
	slackA := &scriptedAdapter{name: "slack", outcome: apperr.Transient("boom", errors.New("net"))} // transient fail
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{"email": emailA, "slack": slackA}})

	err := d.Handle(context.Background(), newTask(t, n.ID))
	if err == nil {
		t.Fatal("expected a retryable error to be returned so Asynq will retry")
	}
	if errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("expected non-SkipRetry error, got %v", err)
	}
	if got := n.Status; got != "partially_sent" {
		t.Errorf("status=%s, want partially_sent (has one success + one transient)", got)
	}
}

// The C1 bug: a retry must NOT re-send channels already marked sent. Seed
// the delivery ledger with email=sent; run the handler again; email
// adapter's Send() must not be called.
func TestDispatcher_AlreadySentChannelIsSkippedOnRetry(t *testing.T) {
	n := seedNotification()
	fs := newFakeStore(n, seedUser())
	sentID := "email:prev-ok"
	fs.deliveries["email"] = &store.Delivery{Channel: "email", Status: "sent", ProviderID: &sentID}

	emailA := &scriptedAdapter{name: "email"} // would succeed; must NOT be called
	slackA := &scriptedAdapter{name: "slack"} // succeeds fresh
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{"email": emailA, "slack": slackA}})

	if err := d.Handle(context.Background(), newTask(t, n.ID)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if emailA.calls != 0 {
		t.Errorf("email adapter called %d times on retry, want 0", emailA.calls)
	}
	if slackA.calls != 1 {
		t.Errorf("slack adapter called %d times, want 1", slackA.calls)
	}
	if n.Status != "sent" {
		t.Errorf("status=%s, want sent (email skipped + slack fresh success)", n.Status)
	}
}

func TestDispatcher_AllPermanentFailures_MovesToDLQ(t *testing.T) {
	n := seedNotification()
	fs := newFakeStore(n, seedUser())
	perm := apperr.Permanent("bounced", errors.New("550"))
	emailA := &scriptedAdapter{name: "email", outcome: perm}
	slackA := &scriptedAdapter{name: "slack", outcome: perm}
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{"email": emailA, "slack": slackA}})

	err := d.Handle(context.Background(), newTask(t, n.ID))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err=%v, want wrapped asynq.SkipRetry", err)
	}
	if n.Status != "dlq" {
		t.Errorf("status=%s, want dlq", n.Status)
	}
}

func TestDispatcher_AllChannelsSucceed_ReturnsNil(t *testing.T) {
	n := seedNotification()
	fs := newFakeStore(n, seedUser())
	emailA := &scriptedAdapter{name: "email"}
	slackA := &scriptedAdapter{name: "slack"}
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{"email": emailA, "slack": slackA}})

	if err := d.Handle(context.Background(), newTask(t, n.ID)); err != nil {
		t.Fatal(err)
	}
	if n.Status != "sent" {
		t.Errorf("status=%s, want sent", n.Status)
	}
}

func TestDispatcher_PartialThenPermanentOnly_FinalizesAsPartiallySent(t *testing.T) {
	// Email succeeds; Slack fails permanently. Nothing transient → task is
	// done, final state is partially_sent, no retry.
	n := seedNotification()
	fs := newFakeStore(n, seedUser())
	emailA := &scriptedAdapter{name: "email"}
	slackA := &scriptedAdapter{name: "slack", outcome: apperr.Permanent("bounced", errors.New("550"))}
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{"email": emailA, "slack": slackA}})

	err := d.Handle(context.Background(), newTask(t, n.ID))
	if err != nil {
		t.Fatalf("expected nil (no more retries needed), got %v", err)
	}
	if n.Status != "partially_sent" {
		t.Errorf("status=%s, want partially_sent", n.Status)
	}
}

func TestDispatcher_UserOptOut_ShrinksChannelSet(t *testing.T) {
	n := seedNotification()
	fs := newFakeStore(n, seedUser())
	fs.prefs = []store.ChannelPreference{{Channel: "email", Enabled: false}}
	emailA := &scriptedAdapter{name: "email"} // must NOT be called
	slackA := &scriptedAdapter{name: "slack"}
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{"email": emailA, "slack": slackA}})

	if err := d.Handle(context.Background(), newTask(t, n.ID)); err != nil {
		t.Fatal(err)
	}
	if emailA.calls != 0 {
		t.Errorf("opted-out channel sent %d times, want 0", emailA.calls)
	}
	if n.Status != "sent" {
		t.Errorf("status=%s, want sent (only slack remained)", n.Status)
	}
}

func TestDispatcher_AllChannelsOptedOut_CancelsNotification(t *testing.T) {
	n := seedNotification()
	fs := newFakeStore(n, seedUser())
	fs.prefs = []store.ChannelPreference{
		{Channel: "email", Enabled: false},
		{Channel: "slack", Enabled: false},
	}
	emailA := &scriptedAdapter{name: "email"}
	slackA := &scriptedAdapter{name: "slack"}
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{"email": emailA, "slack": slackA}})

	if err := d.Handle(context.Background(), newTask(t, n.ID)); err != nil {
		t.Fatal(err)
	}
	if n.Status != "cancelled" {
		t.Errorf("status=%s, want cancelled", n.Status)
	}
	if emailA.calls+slackA.calls != 0 {
		t.Error("no channel should have been called")
	}
}

func TestDispatcher_TerminalRowIsSkipped(t *testing.T) {
	n := seedNotification()
	n.Status = "sent"
	fs := newFakeStore(n, seedUser())
	emailA := &scriptedAdapter{name: "email"}
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{"email": emailA}})

	if err := d.Handle(context.Background(), newTask(t, n.ID)); err != nil {
		t.Fatal(err)
	}
	if emailA.calls != 0 {
		t.Error("already-sent notification must not re-send")
	}
}

// M1: when store writes fail (simulating a brief DB hiccup), the
// dispatcher must NOT fail the task — the network send already happened
// — but it MUST bump the store_write_errors_total counter so the drift
// is observable. Here we drive the success path and inject a store
// error on UpdateNotificationStatus; the handler should return nil and
// the counter should advance.
func TestDispatcher_StoreWriteErrors_AreObservableNotBlocking(t *testing.T) {
	n := seedNotification()
	fs := newFakeStore(n, seedUser())
	fs.updateStatusErr = errors.New("db hiccup")
	emailA := &scriptedAdapter{name: "email"}
	slackA := &scriptedAdapter{name: "slack"}
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{"email": emailA, "slack": slackA}})

	before := readCounter(t, "update_status")
	if err := d.Handle(context.Background(), newTask(t, n.ID)); err != nil {
		t.Fatalf("handle returned error despite write being non-blocking: %v", err)
	}
	after := readCounter(t, "update_status")
	if after <= before {
		t.Errorf("store_write_errors_total did not advance (before=%v after=%v)", before, after)
	}
}

func readCounter(t *testing.T, op string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := observability.StoreWriteErrors.GetMetricWithLabelValues(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func TestDispatcher_NotificationDisappearedMidQueue_SkipRetries(t *testing.T) {
	nid := uuid.New()
	fs := newFakeStore(nil, seedUser())
	d := NewDispatcher(fs, &registryStub{m: map[string]channels.Adapter{}})

	err := d.Handle(context.Background(), newTask(t, nid))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err=%v, want wrapped asynq.SkipRetry", err)
	}
}
