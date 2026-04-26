-- Snapshot template content at notification-creation time so the delivered
-- message is deterministic regardless of later template edits or deletions.
-- Previously, notifications.template_id was ON DELETE SET NULL, which meant
-- a template delete between API acceptance and worker pick-up could flip the
-- worker onto the ad-hoc fallback and emit empty content.
ALTER TABLE notifications
    ADD COLUMN subject_snapshot            TEXT,
    ADD COLUMN body_text_snapshot          TEXT,
    ADD COLUMN body_html_snapshot          TEXT,
    ADD COLUMN template_name_snapshot      TEXT,
    ADD COLUMN required_variables_snapshot TEXT[] NOT NULL DEFAULT '{}';

-- Switch the FK so a template cannot be deleted while it is still referenced
-- by a notification. This also defends against operator error ("DELETE FROM
-- templates WHERE ...") that would otherwise break pending work.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_template_id_fkey;
ALTER TABLE notifications
    ADD CONSTRAINT notifications_template_id_fkey
    FOREIGN KEY (template_id) REFERENCES templates(id) ON DELETE RESTRICT;

-- Backfill: any existing rows take their snapshot from the current template.
-- In production you'd run this as a backfill job with batches + progress
-- tracking; for this service the table is small and the migration is fast.
UPDATE notifications n
   SET subject_snapshot            = t.subject,
       body_text_snapshot          = t.body_text,
       body_html_snapshot          = t.body_html,
       template_name_snapshot      = t.name,
       required_variables_snapshot = t.required_variables
  FROM templates t
 WHERE n.template_id = t.id
   AND n.body_text_snapshot IS NULL;
