-- Partial index backing the reconciler scan. Without this, a
-- millions-row notifications table would pay a sequential scan every
-- 30 seconds. Narrowing to "stuck work only" — typically near-empty
-- — keeps the scan O(stuck_count) regardless of table size.
--
-- Composite (updated_at, id) matches both the WHERE filter's sort order
-- and the compound keyset cursor store.ListStuckQueued uses to paginate.
CREATE INDEX IF NOT EXISTS notifications_stuck_idx
    ON notifications (updated_at, id)
    WHERE status IN ('queued', 'processing');
