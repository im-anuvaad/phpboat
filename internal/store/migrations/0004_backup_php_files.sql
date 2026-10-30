-- docs/architecture.md §7: a backup notes the PHP-looking files it found in DATA_DIR
-- (uploads named *.php, *.phtml, ...), so the app page can warn about them.
-- php_paths holds the first few of them, one per line.
ALTER TABLE backups ADD COLUMN php_files INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN php_paths TEXT    NOT NULL DEFAULT '';
