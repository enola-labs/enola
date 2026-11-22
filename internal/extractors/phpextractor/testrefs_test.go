package phpextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

var _ plugin.TestRefExtractor = (*PHPExtractor)(nil)

func phpTestRefTargets(t *testing.T, relFile, src string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range testRefsFromFile([]byte(src), relFile) {
		if f.Kind != facts.KindTestRef {
			t.Fatalf("emitted a %s fact; only test_ref is allowed", f.Kind)
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls {
				t.Errorf("relation %s -> %s; only calls is allowed", r.Kind, r.Target)
			}
			out[r.Target] = true
		}
	}
	return out
}

func sortedTargets(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestPHPTestRefsCaptureProductionReferences pins what the pass is for: the classes,
// static calls, constructions and instance calls of a PHPUnit test keep an inbound
// edge into production code once tests/ is ignored, each class under the name its
// `use` import or namespace gives it.
func TestPHPTestRefsCaptureProductionReferences(t *testing.T) {
	src := `<?php

namespace Tests\Unit;

use App\Models\Song;
use App\Services\Streamer as StreamerService;
use App\Values\Format;
use function App\Helpers\format_duration;
use PHPUnit\Framework\Attributes\Test;
use Tests\TestCase;

class StreamerTest extends TestCase
{
    private StreamerService $streamer;

    #[Test]
    public function streams(): void
    {
        $song = Song::factory()->create();
        $this->streamer = new StreamerService(new \App\Http\Kernel());
        $this->streamer->stream($song);
        $this->assertSame(Format::MP3, $song->format());
        $this->assertTrue($song instanceof \App\Contracts\Streamable);
        self::assertNotNull(format_duration(12));
        koel_build_id();
        $mock = $this->createMock(StreamerService::class);
        $mock->expects($this->once())->method('stream')->willReturn(true);
    }

    private function helper(): void {}
}
`
	got := phpTestRefTargets(t, "tests/Unit/StreamerTest.php", src)
	for _, want := range []string{
		`App\Models\Song::factory`,    // a static call on an imported class
		`App\Services\Streamer`,       // an aliased import, as a declared type and a construction
		`App\Http\Kernel`,             // a fully-qualified construction
		`stream`,                      // an instance call through a property
		`format`,                      // an instance call on a local
		`App\Values\Format`,           // a class constant names its class
		`App\Contracts\Streamable`,    // an instanceof check
		`App\Helpers\format_duration`, // an imported function
		`koel_build_id`,               // a global helper
	} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedTargets(got))
		}
	}
	for _, unwanted := range []string{
		`Tests\Unit\StreamerTest`,                                           // the file's own class
		`helper`, `assertSame`, `assertTrue`, `assertNotNull`, `createMock`, // $this-> and self:: members
		`expects`, `method`, `willReturn`, `once`, // the mock builder
		`PHPUnit\Framework\Attributes\Test`,             // an attribute is the harness
		`Song`, `Format`, `Streamer`, `StreamerService`, // never the unresolved spelling
	} {
		if got[unwanted] {
			t.Errorf("unexpected reference %q; have %v", unwanted, sortedTargets(got))
		}
	}
}

// TestPHPTestRefsResolveThroughNamespace pins that a class the test names without
// importing it is the one PHP resolves: in the test's own namespace, never a
// same-named production class elsewhere.
func TestPHPTestRefsResolveThroughNamespace(t *testing.T) {
	src := `<?php
namespace Tests\Feature;

use App\Billing\Invoice;

class InvoiceTest extends \PHPUnit\Framework\TestCase
{
    public function test_total()
    {
        $invoice = new Invoice();
        $stub = new FakeGateway();
        $other = new Helpers\Clock();
        Invoice::fromArray([]);
    }
}

class FakeGateway extends \App\Billing\Gateway {}
`
	got := phpTestRefTargets(t, "tests/Feature/InvoiceTest.php", src)
	for _, want := range []string{`App\Billing\Invoice`, `App\Billing\Invoice::fromArray`, `App\Billing\Gateway`, `Tests\Feature\Helpers\Clock`} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedTargets(got))
		}
	}
	for _, unwanted := range []string{`Invoice`, `FakeGateway`, `Tests\Feature\FakeGateway`, `Helpers\Clock`, `Clock`} {
		if got[unwanted] {
			t.Errorf("unexpected reference %q; have %v", unwanted, sortedTargets(got))
		}
	}
}

// TestPHPTestRefsPest covers a Pest file: top-level closures, the expectation chain
// and Pest's own functions.
func TestPHPTestRefsPest(t *testing.T) {
	src := `<?php
use App\Actions\CreateTeam;
use App\Models\User;

beforeEach(function () {
    $this->user = User::factory()->create();
});

it('creates a team', function () {
    $team = (new CreateTeam)->handle($this->user, 'Ops');
    expect($team->name)->toBe('Ops')->and($team->owner())->not->toBeNull();
});

test('helpers', fn () => expect(team_slug('Ops'))->toEqual('ops'));
`
	got := phpTestRefTargets(t, "tests/Feature/TeamTest.php", src)
	for _, want := range []string{`App\Actions\CreateTeam`, `handle`, `App\Models\User::factory`, `owner`, `team_slug`} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedTargets(got))
		}
	}
	for _, unwanted := range []string{`it`, `test`, `expect`, `beforeEach`, `toBe`, `toBeNull`, `toEqual`, `and`} {
		if got[unwanted] {
			t.Errorf("unexpected reference %q; have %v", unwanted, sortedTargets(got))
		}
	}
}

// TestPHPExtractTestRefsScopesToPHP pins the engine contract: one test_ref fact per
// PHP test file, named by its path, and nothing for other files.
func TestPHPExtractTestRefsScopesToPHP(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tests/Unit/AlphaTest.php", "<?php\nuse App\\Alpha;\nclass AlphaTest { function t() { Alpha::make(); } }\n")
	write("tests/Unit/EmptyTest.php", "<?php\nclass EmptyTest { function t() { $this->assertTrue(true); } }\n")
	write("tests/unit/beta.spec.ts", "beta()\n")

	got, err := New().ExtractTestRefs(context.Background(), dir,
		[]string{"tests/Unit/AlphaTest.php", "tests/Unit/EmptyTest.php", "tests/unit/beta.spec.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "tests/Unit/AlphaTest.php" || got[0].Kind != facts.KindTestRef {
		t.Fatalf("want one test_ref for AlphaTest.php, got %+v", got)
	}
	if len(got[0].Relations) != 1 || got[0].Relations[0].Target != `App\Alpha::make` {
		t.Fatalf("want App\\Alpha::make, got %+v", got[0].Relations)
	}
}

// TestPHPTestRefsDropHarnessReceivers pins the receivers whose bare method names
// would vouch for unrelated production methods: a fluent chain on the test case, a
// model factory's builder, and an object a required file returns.
func TestPHPTestRefsDropHarnessReceivers(t *testing.T) {
	src := `<?php
namespace Tests\Feature;

use App\Models\Team;
use Tests\TestCase;

class TeamTest extends TestCase
{
    public function test_store(): void
    {
        $team = Team::factory()->for($this->owner)->create();
        $this->actingAs($team->owner)->post('/teams')->json('data');
        $migration = require base_path('database/migrations/2024_add_teams.php');
        $migration->up();
        $this->service->archive($team);
        $team->refresh();
    }
}
`
	got := phpTestRefTargets(t, "tests/Feature/TeamTest.php", src)
	for _, want := range []string{`App\Models\Team::factory`, `archive`, `refresh`, `base_path`} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedTargets(got))
		}
	}
	for _, unwanted := range []string{`for`, `create`, `actingAs`, `post`, `json`, `up`} {
		if got[unwanted] {
			t.Errorf("unexpected reference %q; have %v", unwanted, sortedTargets(got))
		}
	}
}
