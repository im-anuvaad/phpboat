-- Phase 5 (docs/plan.md §7): a backup holds the whole DATA_DIR, so a row says
-- what it holds. files (databases in the archive) becomes databases. The new
-- columns count the other entries, and content_bytes is the uncompressed
-- size of the files, SQLite snapshots included.
ALTER TABLE backups RENAME COLUMN files TO databases;
ALTER TABLE backups ADD COLUMN plain_files   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN dirs          INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN links         INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN content_bytes INTEGER NOT NULL DEFAULT 0;
