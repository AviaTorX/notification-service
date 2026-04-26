DROP INDEX IF EXISTS notifications_scheduled_idx;
CREATE INDEX notifications_scheduled_idx
    ON notifications (scheduled_for)
    WHERE scheduled_for IS NOT NULL AND status IN ('pending','queued');

ALTER TABLE notifications ALTER COLUMN status SET DEFAULT 'pending';
ALTER TABLE notifications DROP CONSTRAINT notifications_status_check;
ALTER TABLE notifications
    ADD CONSTRAINT notifications_status_check
    CHECK (status IN ('pending','queued','processing','sent','partially_sent','failed','dlq','cancelled'));
