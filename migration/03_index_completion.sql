-- Index completion marker and sweep support.
--
-- `indexed` alone cannot distinguish "the indexer finished this page" from "a
-- worker claimed it and died": both leave indexed = TRUE. `indexed_at` records
-- completion, and the reconciliation sweep reclaims only rows where it is NULL.
-- Without this column the sweep matched every successfully indexed page and
-- deleted its page_word rows every SWEEP_INTERVAL_SECONDS (C-01).
--
-- This file is idempotent so it can be applied to an existing database as well
-- as a fresh one.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'pages'
          AND column_name = 'indexed_at'
    ) THEN
        ALTER TABLE pages ADD COLUMN indexed_at TIMESTAMP;

        -- One-time backfill for databases created before this column existed.
        -- A page that has word rows is completed: stamp it so the first sweep
        -- does not delete the corpus. A claim with no words is a dead worker,
        -- so it is deliberately left NULL and reclaimed normally.
        UPDATE pages p
        SET indexed_at = COALESCE(p.last_index_attempt_at, NOW())
        WHERE p.indexed = TRUE
          AND EXISTS (SELECT 1 FROM page_word pw WHERE pw.page_id = p.id);
    END IF;
END
$$;

-- The sweep scans for open claims twice per tick. A partial index keeps that
-- off the TOAST-heavy heap and only covers rows that can actually be swept.
CREATE INDEX IF NOT EXISTS idx_pages_open_claims
    ON pages (last_index_attempt_at)
    WHERE indexed = TRUE AND indexed_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pages_unindexed_updated
    ON pages (updated_at)
    WHERE indexed = FALSE;

-- (word_id, page_id) already serves every lookup and uniqueness check the
-- single-column index served, so it is pure write cost.
DROP INDEX IF EXISTS idx_page_word_word_id;

-- The monitor orchestrator was removed; nothing reads or writes this table.
DROP TABLE IF EXISTS monitor_state;
