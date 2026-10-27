<?php
// Ignores the request body and answers with 2 MB.
header("Content-Type: text/plain");
echo str_repeat("y", 2 << 20);
