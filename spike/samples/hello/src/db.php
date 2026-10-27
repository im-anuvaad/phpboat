<?php
function db(): PDO {
    static $db;
    if ($db) return $db;
    $db = new PDO("sqlite:" . getenv("DATA_DIR") . "/app.db");
    $db->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
    $db->exec("PRAGMA journal_mode = WAL");
    $db->exec("PRAGMA busy_timeout = 5000");
    $db->exec("PRAGMA foreign_keys = ON");
    $db->exec("PRAGMA synchronous = NORMAL");
    return $db;
}
