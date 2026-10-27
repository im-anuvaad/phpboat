<?php
// Echoes what PHP sees, for the integration test.
header('Content-Type: application/json');
$keys = ['REQUEST_METHOD', 'REQUEST_URI', 'QUERY_STRING', 'SCRIPT_NAME', 'SCRIPT_FILENAME', 'PATH_INFO', 'PHP_SELF',
    'DOCUMENT_ROOT', 'REMOTE_ADDR', 'HTTPS', 'SERVER_NAME', 'SERVER_PORT', 'HTTP_HOST', 'HTTP_X_FORWARDED_FOR', 'HTTP_PROXY'];
$server = [];
foreach ($keys as $k) { $server[$k] = $_SERVER[$k] ?? null; }
echo json_encode([
    'server' => $server,
    'get' => $_GET,
    'post' => $_POST,
    'env' => getenv('PHPBOAT_TEST_ENV'),
    'https_detected' => (!empty($_SERVER['HTTPS']) && $_SERVER['HTTPS'] !== 'off'),
    'php' => PHP_VERSION,
]);
