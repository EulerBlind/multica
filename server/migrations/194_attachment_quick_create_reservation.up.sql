-- This migration originally shipped locally as
-- 165_attachment_quick_create_reservation. ADD COLUMN IF NOT EXISTS and the
-- guarded FK let the uniquely-numbered migration apply to both fresh databases
-- and persistent databases that already recorded the old version.
ALTER TABLE attachment
  ADD COLUMN IF NOT EXISTS quick_create_task_id UUID;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'attachment_quick_create_task_id_fkey'
      AND conrelid = 'attachment'::regclass
  ) THEN
    ALTER TABLE attachment
      ADD CONSTRAINT attachment_quick_create_task_id_fkey
      FOREIGN KEY (quick_create_task_id)
      REFERENCES agent_task_queue(id)
      ON DELETE SET NULL;
  END IF;
END
$$;

COMMENT ON COLUMN attachment.quick_create_task_id IS
  'Transient reservation granting one quick-create task permission to consume this member-owned draft.';

-- Release reservations from every terminal transition, including sweeper and
-- bulk-cancellation queries. A trigger is the only reliable common boundary
-- because task terminal writes are intentionally spread across many queries.
CREATE OR REPLACE FUNCTION release_quick_create_attachments_on_terminal_state()
RETURNS TRIGGER AS $$
BEGIN
  IF NEW.status IN ('completed', 'failed', 'cancelled')
     AND OLD.status IS DISTINCT FROM NEW.status THEN
    UPDATE attachment
    SET quick_create_task_id = NULL
    WHERE quick_create_task_id = NEW.id
      AND lifecycle = 'draft';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_release_quick_create_attachments ON agent_task_queue;
CREATE TRIGGER trg_release_quick_create_attachments
  AFTER UPDATE OF status ON agent_task_queue
  FOR EACH ROW
  EXECUTE FUNCTION release_quick_create_attachments_on_terminal_state();
