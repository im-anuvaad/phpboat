<?php
error_log('hello from error_log');
trigger_error('a user warning', E_USER_WARNING);
echo 'logged';
