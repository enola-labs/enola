package phpextractor

import "testing"

const groupModels = `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
class Group extends Model {
    public function admin() { return $this->belongsTo(Profile::class); }
    public function url() { return ''; }
}
class Story extends Model {
    public function admin() { return $this->belongsTo(Profile::class); }
}
class Profile extends Model {
    public function permalink() { return ''; }
}
`

const groupControllers = `<?php
namespace App\Http;
use App\Models\Group;
use App\Models\Story;
class FederationController {
    public function getGroupObject($id) {
        $group = Group::whereLocal(true)->findOrFail($id);
        $this->mixed(Story::find($id));
        $this->mixed($group);
        $this->unknown($id);
        $this->reassigned($group);
        $this->withDefault($group);
        $this->viaClosure($group);
        $this->viaParent($group);
        return $this->showGroupObject($group);
    }
    public function showGroupObject($group) {
        return \Cache::remember('k', 60, function () use ($group) {
            return [$group->url(), $group->admin->permalink()];
        });
    }
    public function mixed($group) { return $group->admin; }
    public function unknown($group) { return $group->admin; }
    public function reassigned($group) { $group = $group->fresh(); return $group->admin; }
    public function withDefault($group = null) { return $group->admin; }
    public function viaClosure($group) { return $group->admin; }
    public function viaParent($group) { return $group->admin; }
    public function uncalled($group) { return $group->admin; }
    public function elsewhere($group) { return $group->admin; }
}
class GroupController extends FederationController {
    public function show($id) {
        $group = Group::find($id);
        return $this->showGroupObject($group);
    }
    public function viaParent($group) { return parent::viaParent($group); }
    public function call($other) {
        $other->elsewhere(Group::find(1));
        $this->elsewhere(Group::find(1));
        return array_map([$this, 'viaClosure'], []);
    }
}
`

// TestEloquentRelationReadOnAParameterTypedByEveryCaller: an untyped parameter
// is the model every call site of its method passes, when each site's receiver
// names the method and each passes that model: a relation read on it is a use, and
// a call on it binds as a call on a query result does. A parameter given two models,
// a value of unknown type, a default, an assignment in the body, a call by a
// callable array, a parent:: call, a call on a receiver of unknown type or no call
// at all is untyped, and a column-like read on it records nothing.
func TestEloquentRelationReadOnAParameterTypedByEveryCaller(t *testing.T) {
	got := boundCalls(t, `App\Http\FederationController::showGroupObject`, groupModels, groupControllers)
	wantRelationCalls(t, got,
		[]string{`App\Models\Group::admin`, `App\Models\Group::url`, `App\Models\Profile::permalink`}, nil)
	for _, m := range []string{"mixed", "unknown", "reassigned", "withDefault", "viaClosure", "viaParent", "uncalled", "elsewhere"} {
		got := boundCalls(t, `App\Http\FederationController::`+m, groupModels, groupControllers)
		wantRelationCalls(t, got, nil, []string{`App\Models\Group::admin`, `App\Models\Story::admin`})
	}
}
