DROP INDEX IF EXISTS in_app_user_all_idx;
DROP INDEX IF EXISTS in_app_user_unread_idx;

CREATE INDEX in_app_user_unread_idx ON in_app_notifications (user_id, created_at DESC) WHERE read_at IS NULL;
CREATE INDEX in_app_user_all_idx    ON in_app_notifications (user_id, created_at DESC);
