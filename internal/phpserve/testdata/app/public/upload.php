<?php
header('Content-Type: application/json');
$f = $_FILES['f'] ?? null;
echo json_encode(['error' => $f['error'] ?? -1, 'size' => $f['size'] ?? 0, 'md5' => $f ? md5_file($f['tmp_name']) : '', 'field' => $_POST['field'] ?? null]);
