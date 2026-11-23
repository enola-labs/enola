package phpextractor

import (
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// fileRefTargets returns the targets of one relation kind on the file's reference
// fact, nil when the file has none.
func fileRefTargets(ff []facts.Fact, file, kind string) []string {
	var out []string
	for _, f := range ff {
		if f.Kind != facts.KindFileRef || f.Name != file {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == kind {
				out = append(out, rel.Target)
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestBladeTemplateRecordsReferences: the PHP a Blade view runs (echoes, directive
// arguments, @php blocks, @inject) is walked like a function body, and what it calls
// and names rides on the view's file reference fact. View names and text are not code.
func TestBladeTemplateRecordsReferences(t *testing.T) {
	src := `@extends('layouts.app')
@inject('metrics', 'App\Services\MetricsService')
@section('content')
<script>window.pfl = {!! App\Services\LandingService::get() !!}</script>
<p>{{ $log->actionToText() }}</p>
{{-- {{ $hidden->commentedOut() }} --}}
@{{ vueOnly() }}
<span>mail me at someone@if.example</span>
@foreach(\App\Services\PronounService::pronouns() as $val)
  <option>{{ $val }}</option>
@endforeach
@if(\App\Facades\Captcha::active())
  @include('partials.captcha', ['x' => 1])
@endif
@php
  $count = \App\Models\Place::count();
@endphp
<?php echo format_bytes($size); ?>
@endsection
`
	ff := extractFileAST([]byte(src), "resources/views/site/index.blade.php")
	file := "resources/views/site/index.blade.php"
	wantTargets(t, "calls", fileRefTargets(ff, file, facts.RelCalls),
		`App\Services\LandingService::get`, "actionToText", `App\Services\PronounService::pronouns`,
		`App\Facades\Captcha::active`, `App\Models\Place::count`, "format_bytes")
	wantTargets(t, "names", fileRefTargets(ff, file, facts.RelNames), `App\Services\MetricsService`)
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			t.Fatalf("a view declares no symbol, got %s", f.Name)
		}
	}
}

// TestBladeTemplateOnlyForBladeFiles: a plain PHP file is not read as a template.
func TestBladeTemplateOnlyForBladeFiles(t *testing.T) {
	src := "<p>{{ $log->actionToText() }}</p>\n"
	if got := fileRefTargets(extractFileAST([]byte(src), "resources/views/plain.php"), "resources/views/plain.php", facts.RelCalls); got != nil {
		t.Fatalf("a plain .php file is not Blade, got calls %q", got)
	}
}

// TestBladeDirectiveStringRecordsCalls: the PHP a Blade::directive callback returns
// as a string runs in every view using the directive, so its calls are the
// registering method's.
func TestBladeDirectiveStringRecordsCalls(t *testing.T) {
	src := `<?php
namespace App\Providers;

use Illuminate\Support\Facades\Blade;

class CaptchaServiceProvider
{
    public function boot()
    {
        Blade::directive('captchaScripts', function () {
            return "<?php echo app('captcha.manager')->active()->scripts(); ?>";
        });
        Blade::directive('icon', function ($expression) {
            return "<?php echo (new \App\Util\SvgIcon($expression))->toHtml(); ?>";
        });
    }
}
`
	ff := extractFileAST([]byte(src), "app/Providers/CaptchaServiceProvider.php")
	got := relationsOf(t, ff, `App\Providers\CaptchaServiceProvider::boot`, facts.RelCalls)
	for _, want := range []string{"scripts", "active", "app", "toHtml"} {
		if !contains(got, want) {
			t.Fatalf("boot calls %q, want %s among them", got, want)
		}
	}
	wantTargets(t, "instantiates", relationsOf(t, ff, `App\Providers\CaptchaServiceProvider::boot`, facts.RelInstantiates), `App\Util\SvgIcon`)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
