<?php
require __DIR__ . '/../src/db.php';
db()->exec('CREATE TABLE IF NOT EXISTS hits (id INTEGER PRIMARY KEY, path TEXT NOT NULL, at TEXT NOT NULL)');
db()->exec('CREATE TABLE IF NOT EXISTS notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL, at TEXT NOT NULL)');
fwrite(STDERR, "migrated\n");
