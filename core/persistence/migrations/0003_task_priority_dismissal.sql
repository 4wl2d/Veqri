ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0 CHECK(priority >= -100 AND priority <= 100);
ALTER TABLE tasks ADD COLUMN dismissed INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_tasks_queue_priority
ON tasks(status, dismissed, priority DESC, created_at);
