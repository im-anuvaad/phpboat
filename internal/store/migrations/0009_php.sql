-- phpboat phase 3: apps are PHP-FPM pools (docs/plan.md §8).
-- concurrency = pm.max_children. A release's phpboat.toml (docroot,
-- health path, [php] settings) is kept as JSON in releases.manifest; the
-- run column of the binary era stays unused. Phase 4 squashes all
-- migrations into one.
ALTER TABLE apps ADD COLUMN concurrency INTEGER NOT NULL DEFAULT 4;
ALTER TABLE releases ADD COLUMN manifest TEXT NOT NULL DEFAULT '{}';
