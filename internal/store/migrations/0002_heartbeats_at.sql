-- 0002_heartbeats_at: the hourly pruner deletes by `at` alone; the only index so far
-- led with device_id, so the delete was a full scan of the heartbeats table under the
-- write lock.
CREATE INDEX IF NOT EXISTS heartbeats_at ON heartbeats(at);
