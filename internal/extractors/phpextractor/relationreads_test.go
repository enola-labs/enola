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

// TestEloquentRelationPropertyReadIsAUse: reading a relation as a property of a
// receiver of declared type runs that model's relation method. A column of the model,
// a write and a read on a receiver of unknown type record nothing: without the
// model's columns, `$x->status` may be a column.
func TestEloquentRelationPropertyReadIsAUse(t *testing.T) {
	got := relationCalls(t, `App\Services\Notes::update`, relationModels, `<?php
namespace App\Services;
use App\Models\Status;
use App\Models\Media;
class Notes {
    public function update(Status $status, Media $media, $other) {
        $a = $status->mentions->map(fn ($m) => $m->id);
        $b = $media?->owner;
        $c = $other->profile . $other->poll;
        $d = $status->caption;
        $media->author = null;
        return [$a, $b, $c, $d];
    }
}
`)
	wantRelationCalls(t, got, []string{`App\Models\Status::mentions`, `App\Models\Media::owner`},
		[]string{"mentions", "profile", "poll", `App\Models\Status::poll`, `App\Models\Status::profile`,
			`App\Models\Media::profile`, "caption", `App\Models\Status::caption`, `App\Models\Media::author`})
}

// TestEloquentRelationNamedInEagerLoadIsAUse: a relation path named in with(),
// load(), withCount() and friends on a model class or a typed receiver is a use of
// each relation along it; a later segment is looked up on the model the previous
// relation names. On a receiver of unknown type, and as a keyed value that is no
// closure (flash data), it records nothing.
func TestEloquentRelationNamedInEagerLoadIsAUse(t *testing.T) {
	got := relationCalls(t, `App\Services\MediaService::get`, relationModels, `<?php
namespace App\Services;
use App\Models\Media;
use App\Models\Status;
class MediaService {
    public function get($q, Media $media) {
        Media::with('contentCredential', 'status.mentions:id')->first();
        Status::withCount(['poll as polls']);
        Status::with(['parent' => fn ($p) => $p]);
        $q->whereHas('profile');
        Media::with(['owner' => 'Saved']);
        $media->load('author');
        return redirect()->with('caption', 'saved');
    }
}
`)
	wantRelationCalls(t, got, []string{`App\Models\Media::contentCredential`, `App\Models\Media::status`,
		`App\Models\Status::mentions`, `App\Models\Status::poll`, `App\Models\Status::parent`, `App\Models\Media::author`},
		[]string{"profile", `App\Models\Status::profile`, `App\Models\Media::profile`, "owner", `App\Models\Media::owner`,
			"caption", "polls", "status", "mentions"})
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
