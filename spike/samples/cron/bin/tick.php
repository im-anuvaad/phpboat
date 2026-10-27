<?php
// A typical small cron job: some work in SQLite, a line of output.
require __DIR__ . '/../src/db.php';
$rows = 0;
db()->beginTransaction();
for ($i = 0; $i < 2000; $i++) { db()->exec("INSERT INTO runs (at, rows) VALUES ('" . date('c') . "', $i)"); $rows++; }
db()->exec('DELETE FROM runs WHERE id < (SELECT MAX(id) - 10000 FROM runs)');
db()->commit();
echo "tick: wrote $rows rows, peak ", round(memory_get_peak_usage(true) / 1048576, 1), " MB\n";
