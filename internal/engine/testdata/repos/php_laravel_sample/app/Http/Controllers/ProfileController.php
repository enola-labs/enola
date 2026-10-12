<?php

namespace App\Http\Controllers;

use App\Services\UserDirectory;

class ProfileController
{
    public function __construct(private UserDirectory $users)
    {
    }

    public function __invoke(int $id)
    {
        return $this->users->find($id);
    }
}
