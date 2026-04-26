ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_template_id_fkey;
ALTER TABLE notifications
    ADD CONSTRAINT notifications_template_id_fkey
    FOREIGN KEY (template_id) REFERENCES templates(id) ON DELETE SET NULL;

ALTER TABLE notifications
    DROP COLUMN subject_snapshot,
    DROP COLUMN body_text_snapshot,
    DROP COLUMN body_html_snapshot,
    DROP COLUMN template_name_snapshot,
    DROP COLUMN required_variables_snapshot;
