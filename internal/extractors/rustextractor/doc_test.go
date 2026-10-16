package rustextractor

import (
	"context"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractRustRepo(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	repo, rel := writeRustRepo(t, files)
	sort.Strings(rel)
	ff, err := New().Extract(context.Background(), repo, rel)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func docOf(t *testing.T, ff []facts.Fact, kind, name string) (string, bool) {
	t.Helper()
	for _, f := range ff {
		if f.Kind == kind && f.Name == name {
			_, has := f.Prop(facts.PropDoc)
			return f.PropString(facts.PropDoc), has
		}
	}
	t.Fatalf("no %s fact %s", kind, name)
	return "", false
}

// An item's doc is its outer doc comments, read above its attributes; a plain
// comment, a `////` one and one separated by a blank line are none.
func TestRustDoc_Items(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"shop\"\n",
		"src/order.rs": `/// Places an order
/// and charges for it.
///
/// More detail.
#[inline]
#[must_use]
pub fn place() -> u32 { 1 }

// Not a doc.
pub fn plain() {}

//// Not a doc either.
pub fn four() {}

/// Separated.

pub fn loose() {}

/** A cart of picks. */
#[derive(Debug)]
pub struct Cart;

/// Kept for the drop order.
#[allow(dead_code)] // fields read in Drop
struct Guard;

/// What can be bought.
pub enum Item { A }

/// Something that is priced.
pub trait Priced {
    /// The price in cents.
    fn price(&self) -> u32;
}

impl Cart {
    /// Sums the cart.
    pub fn total(&self) -> u32 { 0 }
}

/// Money in cents.
pub type Cents = u32;

/// The most a cart may hold.
pub const MAX: u32 = 10;
`,
	})
	for name, want := range map[string]string{
		"src.place":        "Places an order and charges for it.",
		"src.plain":        "",
		"src.four":         "",
		"src.loose":        "",
		"src.Cart":         "A cart of picks.",
		"src.Guard":        "Kept for the drop order.",
		"src.Item":         "What can be bought.",
		"src.Priced":       "Something that is priced.",
		"src.Priced.price": "The price in cents.",
		"src.Cart.total":   "Sums the cart.",
		"src.Cents":        "Money in cents.",
		"src.MAX":          "The most a cart may hold.",
	} {
		got, has := docOf(t, ff, facts.KindSymbol, name)
		if got != want {
			t.Errorf("%s: doc = %q, want %q", name, got, want)
		}
		if want == "" && has {
			t.Errorf("%s: an undocumented item must carry no doc prop", name)
		}
	}
}

// A module's doc is the inner doc opening lib.rs, main.rs or mod.rs, else the
// file beside its directory; a licence comment and inner attributes may precede it.
func TestRustDoc_Modules(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":       "[package]\nname = \"shop\"\n",
		"src/lib.rs":       "// Licensed under MIT.\n#![deny(missing_docs)]\n//! Orders and payments.\n//!\n//! More detail.\npub mod api;\n",
		"src/api/mod.rs":   "/*! The HTTP surface. */\npub fn serve() {}\n",
		"src/cart.rs":      "//! Picks a customer holds.\npub mod line;\n",
		"src/cart/line.rs": "pub struct Line;\n",
		"src/bare/mod.rs":  "/// An item doc, not the module's.\npub fn f() {}\n",
	})
	for dir, want := range map[string]string{
		"src":      "Orders and payments.",
		"src/api":  "The HTTP surface.",
		"src/cart": "Picks a customer holds.",
		"src/bare": "",
	} {
		got, has := docOf(t, ff, facts.KindModule, dir)
		if got != want {
			t.Errorf("%s: doc = %q, want %q", dir, got, want)
		}
		if want == "" && has {
			t.Errorf("%s: an undocumented module must carry no doc prop", dir)
		}
	}
}
