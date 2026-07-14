-- Partial because only drafts held by an active quick-create task qualify.
-- Keep this separate from migration 194 so the hot attachment table is not
-- write-locked while PostgreSQL builds the lookup/FK-supporting index. IF NOT
-- EXISTS also makes this compatible with persistent databases that applied the
-- same index under 166_attachment_quick_create_reservation_index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_attachment_quick_create_task
  ON attachment(quick_create_task_id)
  WHERE quick_create_task_id IS NOT NULL;
