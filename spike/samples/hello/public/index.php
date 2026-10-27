<?php
require __DIR__ . '/../src/db.php';
$path = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);
if ($path === '/healthz') { echo 'ok'; return; }
if ($path === '/alloc') { $s = str_repeat('x', (int)($_GET['mb'] ?? 1) << 20); usleep(300000); echo 'allocated ', strlen($s) >> 20, ' MB'; return; }
if ($_SERVER['REQUEST_METHOD'] === 'POST') {
    db()->prepare('INSERT INTO notes (body, at) VALUES (?, ?)')->execute([substr($_POST['body'] ?? '', 0, 500), date('c')]);
    header('Location: /', true, 303);
    return;
}
db()->prepare('INSERT INTO hits (path, at) VALUES (?, ?)')->execute([$path, date('c')]);
$hits = db()->query('SELECT COUNT(*) FROM hits')->fetchColumn();
$notes = db()->query('SELECT body, at FROM notes ORDER BY id DESC LIMIT 10')->fetchAll(PDO::FETCH_ASSOC);
?><!doctype html><title>hello</title><link rel="stylesheet" href="/style.css">
<h1>Hello from <?= htmlspecialchars(getenv('PHPBOAT_APP') ?: '?') ?></h1>
<p>Path <code><?= htmlspecialchars($path) ?></code>, hit #<?= $hits ?>, PHP <?= PHP_VERSION ?>,
visitor <?= htmlspecialchars($_SERVER['REMOTE_ADDR']) ?>, https=<?= htmlspecialchars($_SERVER['HTTPS'] ?? 'off') ?>,
env=<?= htmlspecialchars(getenv('SECRET_FROM_DASH') ?: '') ?><?php if (isset($_GET['read'])): ?>, read=<?= @file_get_contents($_GET['read']) === false ? 'blocked' : 'allowed' ?><?php endif ?></p>
<form method="post"><input name="body"><button>Add note</button></form>
<ul><?php foreach ($notes as $n): ?><li><?= htmlspecialchars($n['body']) ?> <small><?= $n['at'] ?></small></li><?php endforeach ?></ul>
