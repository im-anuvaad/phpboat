<?php
require __DIR__ . '/../src/db.php';
db()->exec('CREATE TABLE IF NOT EXISTS runs (id INTEGER PRIMARY KEY, at TEXT NOT NULL, rows INTEGER NOT NULL)');
