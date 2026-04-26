-- In-app listing now uses keyset pagination on the compound (created_at,
-- id) cursor (see migration 0002 + store.ListInAppPage). The existing
-- indexes order by created_at only, which forced Postgres to sort ties
-- in memory whenever a page straddled same-timestamp rows. Rebuild the
-- indexes with a trailing id DESC so the full ORDER BY is satisfied
-- directly from the index. Same operation on both the "all" index and
-- the unread-only partial index.
--
-- DROP+CREATE is safe: both indexes are on a single table, no FK depends
-- on them, and at demo scale the table is small. In production you'd do
-- CREATE INDEX CONCURRENTLY with a temporary second name, then swap.

DROP INDEX IF EXISTS in_app_user_all_idx;
DROP INDEX IF EXISTS in_app_user_unread_idx;

CREATE INDEX in_app_user_all_idx
    ON in_app_notifications (user_id, created_at DESC, id DESC);

CREATE INDEX in_app_user_unread_idx
    ON in_app_notifications (user_id, created_at DESC, id DESC)
    WHERE read_at IS NULL;
