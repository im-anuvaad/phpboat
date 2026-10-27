<?php
http_response_code(201);
setcookie('a', '1');
setcookie('b', '2');
header('X-Custom: yes');
header('Cache-Control: no-store');
echo 'created';
