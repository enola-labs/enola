<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

class User extends Model
{
    protected $table = 'accounts';

    public function photos()
    {
        return $this->hasMany(Photo::class);
    }
}
