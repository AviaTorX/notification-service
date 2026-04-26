-- The notifications.status CHECK included 'pending' and 'failed' since
-- the initial schema, but the Go code never writes either value:
--   - INSERT explicitly sets status = 'queued', so the 'pending' default
--     is unreachable.
--   - The dispatcher routes "all permanent failures" to 'dlq', not
--     'failed', because 'failed' and 'dlq' would otherwise be
--     synonymous; only 'dlq' survived.
--
-- Drop the dead values so schema hygiene matches the Go code and an
-- enum-drift bug can't quietly slip in later. Also change the default
-- to 'queued' to match reality — no row should ever be 'pending'.
ALTER TABLE notifications DROP CONSTRAINT notifications_status_check;
ALTER TABLE notifications
    ADD CONSTRAINT notifications_status_check
    CHECK (status IN ('queued','processing','sent','partially_sent','dlq','cancelled'));
ALTER TABLE notifications ALTER COLUMN status SET DEFAULT 'queued';

-- The existing partial index references status IN ('pending','queued');
-- rebuild without 'pending' so the planner doesn't carry a dead branch.
-- IF NOT EXISTS makes a `migrate force` replay safe — without it, a
-- re-run would collide on the recreated index name.
DROP INDEX IF EXISTS notifications_scheduled_idx;
CREATE INDEX IF NOT EXISTS notifications_scheduled_idx
    ON notifications (scheduled_for)
    WHERE scheduled_for IS NOT NULL AND status = 'queued';
