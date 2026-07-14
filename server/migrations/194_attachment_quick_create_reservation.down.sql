DROP TRIGGER IF EXISTS trg_release_quick_create_attachments ON agent_task_queue;
DROP FUNCTION IF EXISTS release_quick_create_attachments_on_terminal_state();
ALTER TABLE attachment DROP COLUMN IF EXISTS quick_create_task_id;
