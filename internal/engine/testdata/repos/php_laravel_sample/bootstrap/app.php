<?php

use Illuminate\Foundation\Application;
use Illuminate\Support\Facades\Route;

return Application::configure(basePath: dirname(__DIR__))
    ->withRouting(
        then: function () {
            Route::prefix('internal')->group(base_path('routes/internal.php'));
        },
    )->create();
