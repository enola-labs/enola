<?php
use App\Concrete;
function main(): void {
    $c = new Concrete();
    $c->run();
    $c->handle();
}
main();
