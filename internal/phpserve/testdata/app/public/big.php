<?php
$n = (int)($_GET['mb'] ?? 5);
for ($i = 0; $i < $n * 16; $i++) { echo str_repeat(chr(97 + $i % 26), 65536); }
