package phpextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// recursiveBySuffix reports the recursive_self flag of the one symbol whose name ends
// in suffix, failing the test when there is not exactly one.
func recursiveBySuffix(t *testing.T, ff []facts.Fact, suffix string) bool {
	t.Helper()
	var hit []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			hit = append(hit, f)
		}
	}
	if len(hit) != 1 {
		var names []string
		for _, f := range ff {
			names = append(names, f.Name)
		}
		t.Fatalf("want one symbol ending %q, got %d of %v", suffix, len(hit), names)
	}
	return hit[0].Props["recursive_self"] == true
}

func TestPHPRecursion_SameNameIsNotSelfCall(t *testing.T) {
	ff := extractFileAST([]byte(`<?php
class SendJob extends AbstractJob {
    public function __construct($post) {
        parent::__construct();
        $this->post = $post;
    }
}
class ClientPipe {
    public function handle($request) {
        return $this->getPipe()->handle($request);
    }
    public function getPipe() { return $this->pipe; }
}
class Formatter {
    public function render($xml) {
        $renderer = $this->getRenderer();
        return $renderer->render($xml);
    }
    public function extend($container) {
        $container->extend('flarum.formatter', 1);
    }
    public function getRenderer() { return $this->r; }
}
`), "x.php")
	for _, name := range []string{"SendJob::__construct", "ClientPipe::handle", "Formatter::render", "Formatter::extend"} {
		if recursiveBySuffix(t, ff, name) {
			t.Errorf("%s is flagged recursive_self; it calls a different method of the same name", name)
		}
	}
}

func TestPHPRecursion_RealSelfCallsStillFlagged(t *testing.T) {
	ff := extractFileAST([]byte(`<?php
class Tree {
    public function walk($n) {
        foreach ($n->children as $c) { $this->walk($c); }
    }
    public static function depth($n) {
        return $n ? 1 + self::depth($n->parent) : 0;
    }
}
`), "x.php")
	for _, name := range []string{"Tree::walk", "Tree::depth"} {
		if !recursiveBySuffix(t, ff, name) {
			t.Errorf("%s calls itself and is not flagged recursive_self", name)
		}
	}
}

// TestPHPRecursion_GlobalFunctionCallInMethodIsNotSelfCall: a plain function call
// never runs a method, so a model accessor named after a framework helper
// (`url()`, `config()`) does not recurse by calling the helper.
func TestPHPRecursion_GlobalFunctionCallInMethodIsNotSelfCall(t *testing.T) {
	ff := extractFileAST([]byte(`<?php
namespace App\Models;
class Profile {
    public function url($suffix = null) {
        return $this->remote_url ?? url($this->username . $suffix);
    }
    public function config() {
        foreach ($this->keys as $k) { $out[] = config($k); }
        return $out;
    }
}
`), "app/Models/Profile.php")
	for _, name := range []string{"Profile::url", "Profile::config"} {
		if recursiveBySuffix(t, ff, name) {
			t.Errorf("%s is flagged recursive_self; it calls the global function of its name", name)
		}
	}
	calls, _ := symbolBySuffix(t, ff, "Profile::config").Props["calls_in_loop"].([]string)
	if len(calls) != 1 || calls[0] != "config" {
		t.Errorf("Profile::config calls_in_loop = %v, want [config]", calls)
	}
}

// TestPHPRecursion_FunctionCallingItselfStillFlagged: a function calling its own
// name, in a namespace or not, recurses.
func TestPHPRecursion_FunctionCallingItselfStillFlagged(t *testing.T) {
	ff := extractFileAST([]byte(`<?php
namespace App\Support;
function flatten($a) {
    return is_array($a) ? array_merge(...array_map(fn ($x) => flatten($x), $a)) : [$a];
}
`), "app/Support/helpers.php")
	if !recursiveBySuffix(t, ff, `App\Support\flatten`) {
		t.Error("flatten calls itself and is not flagged recursive_self")
	}
}

func symbolBySuffix(t *testing.T, ff []facts.Fact, suffix string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			return f
		}
	}
	t.Fatalf("no symbol ending %q", suffix)
	return facts.Fact{}
}
