package rustextractor

import (
	"testing"

	sitter "github.com/tree-sitter/go-tree-sitter"
	rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
)

const nodeKindSrc = `extern crate serde;

use std::collections::HashMap;
use std::io::{Read, Write};
use std::fmt::*;
use std::sync::atomic::{self, AtomicUsize};
use crate::inner::Thing;
use super::parent::Other;
use std::rc::Rc as Shared;

#[derive(Debug)]
pub struct Point { pub x: i32, pub y: i32 }

pub enum Color { Red, Green }

pub const MAX: i32 = 10;
pub static NAME: &str = "svc";
pub type Alias = u32;

pub trait Handler {
    fn handle(&self, s: &str) -> bool;
}

pub mod inner {
    pub struct Thing;
}

macro_rules! twice {
    ($x:expr) => { $x + $x };
}

impl Handler for Point {
    fn handle(&self, s: &str) -> bool {
        let p = Point { x: 1, y: 2 };
        let xs = [1, 2, 3];
        let mut total = 0;
        let r = &total;
        let raw = r"raw";
        let text: &str = "plain";
        for i in 0..xs.len() {
            total += i as i32;
        }
        if total > 0 { total = 1; }
        while total > 0 { total -= 1; }
        loop { break; }
        let m = match total {
            0 => true,
            _ => false,
        };
        let v = parse::<i32>("1");
        let t = (1, 2);
        println!("{} {} {}", s, raw, text);
        twice!(total);
        m && p.x > 0 && true && *r == 0
    }
}

impl Point {
    fn load(&self) -> Result<i32, std::io::Error> {
        let v = self.compute()?;
        Ok(v)
    }
}

extern "C" { fn Foreign(x: i32) -> i32; }
fn callback() {}
fn forms() {
    let callback = callback as *const ();
    let _ = CallbackHolder { callback };
}
struct CallbackHolder { callback: fn() }
`

// TestWalkerNodeKindsStillExist pins the exact grammar node kinds the walker dispatches
// on. A grammar upgrade that renamed one would otherwise degrade extraction silently —
// the walker descends into anything it does not recognize, so a renamed declaration node
// stops producing symbols without producing a single error.
//
// The list is every string this package switches on (or compares Kind() against) that is
// a real node type in the grammar; the sources below are built to produce all of them.
func TestWalkerNodeKindsStillExist(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(rust.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse([]byte(nodeKindSrc), nil)
	defer tree.Close()
	if tree.RootNode().HasError() {
		t.Fatalf("the pinning source no longer parses cleanly:\n%s", tree.RootNode().ToSexp())
	}
	seen := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		seen[n.Kind()] = true
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())
	want := []string{
		"arguments", "array_expression", "attribute", "attribute_item", "binary_expression",
		"call_expression", "const_item", "crate", "enum_item", "extern_crate_declaration",
		"field_expression", "field_identifier", "field_initializer", "for_expression",
		"foreign_mod_item", "function_item", "function_signature_item", "generic_function", "generic_type",
		"identifier", "if_expression", "impl_item", "integer_literal", "loop_expression",
		"match_arm", "metavariable", "mod_item", "range_expression", "raw_string_literal",
		"reference_expression", "reference_type", "scoped_identifier", "scoped_type_identifier",
		"scoped_use_list", "self", "self_parameter", "static_item", "string_content",
		"shorthand_field_initializer", "string_literal", "struct_expression", "struct_item", "super", "token_tree", "type_cast_expression",
		"trait_item", "true", "try_expression", "tuple_expression", "type_identifier",
		"type_item", "use_as_clause", "use_declaration", "use_list", "use_wildcard",
		"while_expression",
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("grammar no longer produces node kind %q — the walker dispatches on it "+
				"and would silently stop extracting", k)
		}
	}
}

// TestGrammarSmoke is the ABI guard, and it is the most important test in this package.
//
// The vendored go-tree-sitter runtime accepts at most tree-sitter ABI 14. A grammar
// generated against ABI 15 is refused by SetLanguage, and the extractor returns nil on
// that error — so the rejection is SILENT: every Rust file parses to nothing, and the
// fact graph becomes indistinguishable from a repository containing none. That is exactly
// how the C# grammar failed once (see dotnetextractor/csharp.go).
//
// tree-sitter-rust is pinned at v0.23.3, the last ABI-14 release; v0.24.0 and later are
// ABI 15. The bound is recorded in .github/dependabot.yml.
//
// If this fails after a dependency bump, the fix is to pin the grammar back to its last
// ABI-14 release, not to loosen the assertion.
func TestGrammarSmoke(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(rust.Language())); err != nil {
		t.Fatalf("SetLanguage failed — the grammar is almost certainly built against a "+
			"newer tree-sitter ABI than the vendored runtime accepts. Error: %v", err)
	}

	// Each case exercises a different half of the grammar, so a regeneration that broke
	// one would not pass on the others.
	for _, tc := range []struct{ name, src string }{
		{"mod_struct_impl", "pub mod inner {\n    pub struct Point { pub x: i32 }\n}\n\nimpl inner::Point {\n    pub fn new() -> Self { Self { x: 0 } }\n}\n"},
		{"traits_generics", "pub trait Handler<T> {\n    fn handle(&self, v: T) -> Result<T, String>;\n}\n\nimpl<T: Clone> Handler<T> for Svc {\n    fn handle(&self, v: T) -> Result<T, String> { Ok(v) }\n}\n"},
		{"macros_and_use", "use std::collections::{HashMap, HashSet};\n\nmacro_rules! twice {\n    ($x:expr) => { $x + $x };\n}\n\nfn f() { println!(\"{}\", twice!(1)); }\n"},
		{"async_await", "pub async fn load(url: &str) -> Result<String, Error> {\n    let body = get(url).await?.text().await?;\n    Ok(body)\n}\n"},
		{"axum_routes", "pub fn app() -> Router {\n    Router::new()\n        .route(\"/health\", get(health))\n        .nest(\"/api\", api_router())\n}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := parser.Parse([]byte(tc.src), nil)
			if tree == nil {
				t.Fatal("nil tree")
			}
			defer tree.Close()
			root := tree.RootNode()
			if root == nil {
				t.Fatal("nil root node")
			}
			if root.HasError() {
				t.Errorf("a trivial valid file parsed with errors — grammar mismatch:\n%s", root.ToSexp())
			}
			// A grammar the runtime refused yields a root with no children rather than an
			// error, so HasError alone would not catch it.
			if root.ChildCount() == 0 {
				t.Error("root has no children — the grammar was probably rejected")
			}
		})
	}
}
