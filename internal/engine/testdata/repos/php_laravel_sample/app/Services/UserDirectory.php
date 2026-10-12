<?php

namespace App\Services;

use App\Models\User;

class UserDirectory
{
    public function find(int $id)
    {
        return User::query()->findOrFail($id);
    }
}
