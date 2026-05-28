-- Migrate messages_by_room to TimeWindowCompactionStrategy (TWCS).
--
-- Default (STCS) groups SSTables by size, so cold historical data gets
-- rewritten alongside the hot recent stuff every time a tier rebalances.
-- For this bucketed time-series table that's pure waste: compaction CPU
-- scales with total table size instead of recent write volume.
--
-- TWCS groups SSTables by time window. Once a window is sealed, its
-- SSTables are compacted once and then left alone (until they age out via
-- TTL or DELETE). compaction_window_size MUST match MESSAGE_BUCKET_HOURS
-- so each Cassandra window aligns with one logical bucket. The value
-- below matches the services' MESSAGE_BUCKET_HOURS envDefault of 72
-- (see message-worker/main.go and history-service/internal/config/config.go).
-- If your deployment overrides MESSAGE_BUCKET_HOURS to a different value,
-- change both numbers together.
--
-- thread_messages_by_thread is intentionally NOT migrated: it is
-- partitioned per thread (thread_room_id), not time-bucketed, so the
-- window-alignment rationale does not apply. It keeps the default strategy.
--
-- Apply manually against the production keyspace; the docker-local init
-- scripts already create new clusters with these settings.
--
-- ALTER does not rewrite existing SSTables. New compactions use TWCS;
-- pre-existing STCS-compacted SSTables remain until they're recompacted,
-- so the benefit materialises gradually over hours/days.
--
-- Caveats before running this in production:
--   * Late-arriving writes (federation replay from inbox-worker, backfills,
--     any USING TIMESTAMP <old>) land in the current TWCS window and create
--     small SSTables outside their natural window. Tolerable in small
--     volume; investigate first if federation lag routinely exceeds the
--     window size.
--   * Avoid full-cluster `nodetool repair` on TWCS tables — it shovels old
--     data back into the current window. Use sub-range / incremental repair.

ALTER TABLE chat.messages_by_room WITH compaction = {
  'class': 'TimeWindowCompactionStrategy',
  'compaction_window_unit': 'HOURS',
  'compaction_window_size': '72'
};
