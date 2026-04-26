package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Integration tests against a real Postgres. Gated on TEST_DB_URL so the
// default `go test ./...` run stays infra-free; point the env var at the
// docker-compose Postgres (default localhost:55432) to run them.
//
// These tests cover the code paths the reviewer flagged as under-tested,
// specifically:
//   - CreateNotification idempotency key collision returns
//     ErrIdempotencyKeyReused (not a bare 500) — B3.
//   - ListStuckQueued picks up rows stuck in queued/processing — B2.
//   - Template snapshot columns round-trip — C2.
//   - ListInAppPage keyset pagination yields the right ordering + next
//     cursor — G2.
func gateDB(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		dsn = "postgres://notif:notifpass@localhost:55432/notifications?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := New(ctx, dsn, PoolOptions{})
	if err != nil {
		t.Skipf("postgres unavailable at %s (set TEST_DB_URL or run docker-compose): %v", dsn, err)
	}
	return s
}

func seedUser(t *testing.T, s *Store) *User {
	t.Helper()
	email := "test+" + uuid.NewString()[:8] + "@example.com"
	u, err := s.UpsertUser(context.Background(), UpsertUserParams{
		ID:    uuid.New(),
		Email: &email,
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestCreateNotification_IdempotencyKeyCollision(t *testing.T) {
	s := gateDB(t)
	defer s.Close()
	ctx := context.Background()
	u := seedUser(t, s)
	key := "int-test-" + uuid.NewString()

	body := "hi"
	name := "adhoc"
	p := CreateNotificationParams{
		ID:                   uuid.New(),
		IdempotencyKey:       &key,
		UserID:               u.ID,
		Channels:             []string{"email"},
		Variables:            map[string]any{},
		BodyTextSnapshot:     &body,
		TemplateNameSnapshot: &name,
	}
	first, err := s.CreateNotification(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// Second insert with the same key but a different id must collide.
	p2 := p
	p2.ID = uuid.New()
	_, err = s.CreateNotification(ctx, p2)
	if !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("second insert err=%v, want ErrIdempotencyKeyReused", err)
	}

	// Replay via lookup returns the original row.
	existing, err := s.GetNotificationByIdempotencyKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if existing.ID != first.ID {
		t.Errorf("lookup returned id=%s, want %s", existing.ID, first.ID)
	}
	if existing.BodyTextSnapshot == nil || *existing.BodyTextSnapshot != "hi" {
		t.Errorf("snapshot not persisted")
	}
}

func TestListStuckQueued_PicksUpOldQueuedRows(t *testing.T) {
	s := gateDB(t)
	defer s.Close()
	ctx := context.Background()
	u := seedUser(t, s)

	body := "hi"
	name := "adhoc"
	n, err := s.CreateNotification(ctx, CreateNotificationParams{
		ID:                   uuid.New(),
		UserID:               u.ID,
		Channels:             []string{"email"},
		Variables:            map[string]any{},
		BodyTextSnapshot:     &body,
		TemplateNameSnapshot: &name,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Backdate updated_at so the reconciler window sees it. Direct SQL is
	// acceptable here because the test owns the data lifecycle.
	_, err = s.Pool.Exec(ctx, `UPDATE notifications SET updated_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, n.ID)
	if err != nil {
		t.Fatal(err)
	}

	rows, _, err := s.ListStuckQueued(ctx, StuckQueuedParams{
		OlderThan: 5 * time.Minute,
		Limit:     100,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.ID == n.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("reconciler query missed a queued row older than stuckAfter")
	}
}

// K1: a notification with scheduled_for far in the future must NOT be
// returned by the reconciler scan. Previously it was, which made every
// scheduled notification trigger a re-enqueue attempt (and an
// ErrTaskIDConflict) on every reconciler tick.
func TestListStuckQueued_SkipsFutureScheduledRows(t *testing.T) {
	s := gateDB(t)
	defer s.Close()
	ctx := context.Background()
	u := seedUser(t, s)

	body, name := "hi", "adhoc"
	far := time.Now().Add(24 * time.Hour)
	farRow, err := s.CreateNotification(ctx, CreateNotificationParams{
		ID:                   uuid.New(),
		UserID:               u.ID,
		Channels:             []string{"email"},
		Variables:            map[string]any{},
		BodyTextSnapshot:     &body,
		TemplateNameSnapshot: &name,
		ScheduledFor:         &far,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Make it look stale by updated_at.
	_, err = s.Pool.Exec(ctx,
		`UPDATE notifications SET updated_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, farRow.ID)
	if err != nil {
		t.Fatal(err)
	}

	rows, _, err := s.ListStuckQueued(ctx, StuckQueuedParams{
		OlderThan: 5 * time.Minute,
		Limit:     100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == farRow.ID {
			t.Fatalf("future-scheduled row (scheduled_for=%v) was returned as stuck", farRow.ScheduledFor)
		}
	}
}

// A notification whose scheduled_for is within one stuck-window of now
// IS stuck — its fire-time should have passed or is about to, and if
// it's not in Redis we want to recover it.
func TestListStuckQueued_IncludesImminentScheduledRows(t *testing.T) {
	s := gateDB(t)
	defer s.Close()
	ctx := context.Background()
	u := seedUser(t, s)

	body, name := "hi", "adhoc"
	imminent := time.Now().Add(-time.Minute) // already should have fired
	row, err := s.CreateNotification(ctx, CreateNotificationParams{
		ID:                   uuid.New(),
		UserID:               u.ID,
		Channels:             []string{"email"},
		Variables:            map[string]any{},
		BodyTextSnapshot:     &body,
		TemplateNameSnapshot: &name,
		ScheduledFor:         &imminent,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool.Exec(ctx,
		`UPDATE notifications SET updated_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, row.ID)
	if err != nil {
		t.Fatal(err)
	}

	rows, _, err := s.ListStuckQueued(ctx, StuckQueuedParams{
		OlderThan: 5 * time.Minute,
		Limit:     100,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.ID == row.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("past-scheduled stuck row was excluded; reconciler can't recover it")
	}
}

// L5: pagination across same-updated_at rows must return every stuck row
// exactly once.
func TestListStuckQueued_Paginates(t *testing.T) {
	s := gateDB(t)
	defer s.Close()
	ctx := context.Background()
	u := seedUser(t, s)

	body := "hi"
	name := "adhoc"
	const n = 7
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		row, err := s.CreateNotification(ctx, CreateNotificationParams{
			ID:                   uuid.New(),
			UserID:               u.ID,
			Channels:             []string{"email"},
			Variables:            map[string]any{},
			BodyTextSnapshot:     &body,
			TemplateNameSnapshot: &name,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.ID)
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE notifications SET updated_at = NOW() - INTERVAL '1 hour' WHERE id = ANY($1)`, ids)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[uuid.UUID]int{}
	cursor := StuckCursor{}
	for i := 0; i < 10; i++ {
		rows, next, err := s.ListStuckQueued(ctx, StuckQueuedParams{
			OlderThan: 5 * time.Minute,
			Limit:     2,
			After:     cursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen[r.ID]++
		}
		if next.IsZero() {
			break
		}
		cursor = next
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("row %s seen %d times (want 1)", id, seen[id])
		}
	}
}

func TestListInAppPage_CursorPagination(t *testing.T) {
	s := gateDB(t)
	defer s.Close()
	ctx := context.Background()
	u := seedUser(t, s)

	// Insert 5 in-app notifications.
	for i := 0; i < 5; i++ {
		if _, err := s.CreateInApp(ctx, CreateInAppParams{
			UserID: u.ID,
			Title:  "t",
			Body:   "b",
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	first, next1, err := s.ListInAppPage(ctx, ListInAppParams{UserID: u.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("first page len=%d, want 2", len(first))
	}
	if next1.IsZero() {
		t.Fatal("next_cursor should be set when more rows exist")
	}

	second, next2, err := s.ListInAppPage(ctx, ListInAppParams{UserID: u.ID, Limit: 2, Before: next1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 {
		t.Fatalf("second page len=%d, want 2", len(second))
	}
	// Cursor must have moved forward (strictly older by the (created_at, id)
	// tuple ordering the store uses).
	if !second[0].CreatedAt.Before(first[len(first)-1].CreatedAt) &&
		!second[0].CreatedAt.Equal(first[len(first)-1].CreatedAt) {
		t.Errorf("second page older row %v >= first page oldest %v",
			second[0].CreatedAt, first[len(first)-1].CreatedAt)
	}

	third, next3, err := s.ListInAppPage(ctx, ListInAppParams{UserID: u.ID, Limit: 2, Before: next2})
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 1 {
		t.Fatalf("third page len=%d, want 1 (5 total - 4 already returned)", len(third))
	}
	if !next3.IsZero() {
		t.Errorf("next_cursor should be zero on last page, got %v", next3)
	}
}

// TestListInAppPage_HandlesSameTimestampRows covers the bug the reviewer
// called out as E4: with a plain `created_at < before` cursor, concurrent
// inserts sharing a timestamp could be skipped. The compound (created_at,
// id) cursor must return every row exactly once across the pages.
func TestListInAppPage_HandlesSameTimestampRows(t *testing.T) {
	s := gateDB(t)
	defer s.Close()
	ctx := context.Background()
	u := seedUser(t, s)

	// Force-insert 4 rows that share a single created_at. Using direct SQL
	// because CreateInApp uses NOW() which spreads rows across microseconds.
	ts := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	for _, id := range ids {
		_, err := s.Pool.Exec(ctx, `
			INSERT INTO in_app_notifications (id, user_id, title, body, data, created_at)
			VALUES ($1, $2, 'ts-clash', 'b', '{}'::jsonb, $3)`, id, u.ID, ts)
		if err != nil {
			t.Fatal(err)
		}
	}

	seen := map[uuid.UUID]struct{}{}
	cursor := InAppCursor{}
	for i := 0; i < 6; i++ { // safety bound: at most 6 pages for 4 rows
		page, next, err := s.ListInAppPage(ctx, ListInAppParams{
			UserID: u.ID,
			Limit:  2,
			Before: cursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range page {
			if _, dup := seen[n.ID]; dup {
				t.Fatalf("row %s returned on two pages", n.ID)
			}
			seen[n.ID] = struct{}{}
		}
		if next.IsZero() {
			break
		}
		cursor = next
	}
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			t.Errorf("row %s was skipped by pagination", id)
		}
	}
}

// TestDeleteTemplate_ReferencedBlocked covers E3: deleting a template that
// still has notifications referencing it must return ErrStillReferenced
// (mapped to 409 by the API), not a raw pg error (5xx).
func TestDeleteTemplate_ReferencedBlocked(t *testing.T) {
	s := gateDB(t)
	defer s.Close()
	ctx := context.Background()
	u := seedUser(t, s)

	tmpl, err := s.UpsertTemplate(ctx, UpsertTemplateParams{
		Name:     "int-test-" + uuid.NewString()[:8],
		BodyText: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	tid := tmpl.ID
	body := "hi"
	name := tmpl.Name
	_, err = s.CreateNotification(ctx, CreateNotificationParams{
		ID:                   uuid.New(),
		UserID:               u.ID,
		TemplateID:           &tid,
		Channels:             []string{"email"},
		Variables:            map[string]any{},
		BodyTextSnapshot:     &body,
		TemplateNameSnapshot: &name,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.DeleteTemplate(ctx, tid)
	if !errors.Is(err, ErrStillReferenced) {
		t.Fatalf("DeleteTemplate err=%v, want ErrStillReferenced", err)
	}
}
