package phpextractor

import (
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const relationModels = `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
class Status extends Model {
    public function mentions() { return $this->hasMany(Mention::class); }
    public function poll() { return $this->hasOne(Poll::class); }
    public function parent() { return $this->morphTo(); }
    public function caption() { return $this->caption_text; }
    public function profile() { return $this->belongsTo(Profile::class); }
    public function author() { return $this->belongsTo(Profile::class); }
}
class Media extends Model {
    public function contentCredential() { return $this->hasOne(MediaContentCredential::class); }
    public function status() { return $this->belongsTo(Status::class); }
    public function owner() { return $this->belongsTo(Profile::class); }
    public function profile() { return $this->belongsTo(Profile::class); }
    public function author() { return $this->belongsTo(Profile::class); }
}
`

// relationCalls runs the symbol walk and the cross-file passes over the sources, and
// returns the calls edges of the named symbol.
func relationCalls(t *testing.T, name string, sources ...string) []string {
	t.Helper()
	var all []facts.Fact
	for i, src := range sources {
		all = append(all, extractFileAST([]byte(src), "app/f"+string(rune('a'+i))+".php")...)
	}
	bindRelationReads(all)
	var out []string
	found := false
	for _, f := range all {
		for _, p := range []string{propPropertyReads, propRelationMethod} {
			if _, left := f.Props[p]; left {
				t.Errorf("%s still carries %s after binding", f.Name, p)
			}
		}
		if f.Kind != facts.KindSymbol || f.Name != name {
			continue
		}
		found = true
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelCalls {
				out = append(out, rel.Target)
			}
		}
	}
	if !found {
		t.Fatalf("no symbol %s", name)
	}
	sort.Strings(out)
	return out
}

// TestEloquentRelationPropertyReadIsAUse: reading a relation as a property runs the
// relation method. A typed receiver binds to the model's method; an untyped one to
// the one relation of that name, else by bare name as an untyped method call is,
// unless a function has that name. A column or a write records nothing.
func TestEloquentRelationPropertyReadIsAUse(t *testing.T) {
	got := relationCalls(t, `App\Services\Notes::update`, relationModels, `<?php
namespace App\Services;
use App\Models\Status;
class Notes {
    public function update(Status $status, $other) {
        $a = $status->mentions->map(fn ($m) => $m->id);
        $b = $other?->poll;
        $c = $other->profile;
        $d = $status->caption;
        $e = $other->profile_id . $other->author;
        $other->owner = null;
        return [$a, $b, $c, $d, $e];
    }
}
`, "<?php\nfunction author() { return null; }\n")
	wantRelationCalls(t, got, []string{`App\Models\Status::mentions`, `App\Models\Status::poll`, "profile"},
		[]string{"mentions", "poll", "caption", `App\Models\Status::caption`, "profile_id",
			"author", `App\Models\Status::author`, `App\Models\Media::author`, `App\Models\Media::owner`})
}

// TestEloquentRelationNamedInEagerLoadIsAUse: a relation named in with(), load(),
// whereHas() and friends, dotted paths per segment, is a use; on a model class the
// first segment binds to that model's method. A key/value with() (a redirect's
// flash data) names no relation.
func TestEloquentRelationNamedInEagerLoadIsAUse(t *testing.T) {
	got := relationCalls(t, `App\Services\MediaService::get`, relationModels, `<?php
namespace App\Services;
use App\Models\Media;
class MediaService {
    public function get($q) {
        Media::with('contentCredential')->first();
        $q->whereHas('status.mentions', fn ($s) => $s)->withCount(['poll as polls'])->with(['parent' => fn ($p) => $p]);
        redirect()->back()->with(['owner' => 'Saved']);
        return redirect()->with('caption', 'saved');
    }
}
`)
	wantRelationCalls(t, got, []string{`App\Models\Media::contentCredential`, `App\Models\Media::status`,
		`App\Models\Status::mentions`, `App\Models\Status::poll`, `App\Models\Status::parent`},
		[]string{"caption", "owner", `App\Models\Media::owner`, "polls", "status.mentions"})
}

// wantRelationCalls checks that got holds every name in want and none in absent.
func wantRelationCalls(t *testing.T, got, want, absent []string) {
	t.Helper()
	has := map[string]bool{}
	for _, g := range got {
		has[g] = true
	}
	for _, w := range want {
		if !has[w] {
			t.Errorf("calls %q lack %s", got, w)
		}
	}
	for _, a := range absent {
		if has[a] {
			t.Errorf("calls %q hold %s", got, a)
		}
	}
}
