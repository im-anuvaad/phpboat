<?php
require __DIR__ . '/../vendor/autoload.php';
$templates = new League\Plates\Engine(__DIR__ . '/../templates');
$dispatcher = FastRoute\simpleDispatcher(function (FastRoute\RouteCollector $r) {
    $r->get('/', fn() => ['home', ['title' => 'Home']]);
    $r->get('/hello/{name}', fn($v) => ['hello', ['title' => 'Hello', 'name' => $v['name']]]);
    $r->get('/healthz', fn() => null);
});
$info = $dispatcher->dispatch($_SERVER['REQUEST_METHOD'], parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH));
if ($info[0] !== FastRoute\Dispatcher::FOUND) { http_response_code(404); echo 'not found'; return; }
$page = $info[1]($info[2] ?? []);
if ($page === null) { echo 'ok'; return; }
echo $templates->render($page[0], $page[1]);
