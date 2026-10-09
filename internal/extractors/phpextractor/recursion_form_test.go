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
