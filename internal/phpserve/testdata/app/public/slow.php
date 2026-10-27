<?php
usleep((int)($_GET['ms'] ?? 200) * 1000);
echo 'slow done';
