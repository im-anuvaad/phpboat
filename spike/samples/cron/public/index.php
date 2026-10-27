<?php
require __DIR__ . '/../src/db.php';
$last = db()->query('SELECT at, COUNT(*) AS n FROM runs GROUP BY at ORDER BY at DESC LIMIT 5')->fetchAll(PDO::FETCH_ASSOC);
?><!doctype html><title>cron</title><h1>Last cron runs</h1>
<ul><?php foreach ($last as $r): ?><li><?= $r['at'] ?>: <?= $r['n'] ?> rows</li><?php endforeach ?></ul>
