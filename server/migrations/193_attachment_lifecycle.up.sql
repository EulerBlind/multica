-- This migration originally shipped locally as 164_attachment_lifecycle.
-- Persistent environments may therefore already have the column while the
-- migration runner has not recorded this uniquely-numbered version. Keep the
-- upgrade idempotent so those databases self-heal exactly like migration 164's
-- attachment_task_id renumber compatibility.
ALTER TABLE attachment
  ADD COLUMN IF NOT EXISTS lifecycle TEXT;

ALTER TABLE attachment
  ALTER COLUMN lifecycle SET DEFAULT 'draft';

-- Existing bound rows predate the lifecycle column. Treat every durable
-- binding as committed during the backfill so an upgrade can never make an
-- existing attachment eligible for draft cleanup.
UPDATE attachment
SET lifecycle = 'committed'
WHERE issue_id IS NOT NULL
   OR comment_id IS NOT NULL
   OR chat_message_id IS NOT NULL;

UPDATE attachment
SET lifecycle = 'draft'
WHERE lifecycle IS NULL;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'attachment_lifecycle_check'
      AND conrelid = 'attachment'::regclass
  ) THEN
    ALTER TABLE attachment
      ADD CONSTRAINT attachment_lifecycle_check
      CHECK (lifecycle IN ('draft', 'committed'));
  END IF;
END
$$;

ALTER TABLE attachment
  ALTER COLUMN lifecycle SET NOT NULL;
