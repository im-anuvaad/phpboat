<?php
$body = file_get_contents('php://input');
echo strlen($body), ' ', md5($body);
