<?php
$dir = __DIR__ . '/uploads';
$msg = '';
if ($_SERVER['REQUEST_METHOD'] === 'POST' && isset($_FILES['f']) && $_FILES['f']['error'] === UPLOAD_ERR_OK) {
    // deliberately naive: keeps the original extension, so a .php upload is possible (the host must never run it)
    $name = preg_replace('/[^A-Za-z0-9._-]/', '_', basename($_FILES['f']['name']));
    move_uploaded_file($_FILES['f']['tmp_name'], "$dir/$name") or http_response_code(500);
    $msg = "stored $name";
}
$files = array_values(array_filter(scandir($dir), fn($f) => $f[0] !== '.'));
?><!doctype html><title>uploads</title>
<p><?= htmlspecialchars($msg) ?></p>
<form method="post" enctype="multipart/form-data"><input type="file" name="f"><button>Upload</button></form>
<ul><?php foreach ($files as $f): ?><li><a href="/uploads/<?= rawurlencode($f) ?>"><?= htmlspecialchars($f) ?></a></li><?php endforeach ?></ul>
