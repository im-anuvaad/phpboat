<?php
header('Content-Type: text/event-stream');
while (ob_get_level()) { ob_end_flush(); }
echo "data: one\n\n"; flush();
usleep(400000);
echo "data: two\n\n"; flush();
