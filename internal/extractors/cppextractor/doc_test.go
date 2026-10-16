package cppextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func docBySuffix(t *testing.T, ff []facts.Fact, want map[string]string) {
	t.Helper()
	for suffix, w := range want {
		var f *facts.Fact
		for i := range ff {
			if ff[i].Kind == facts.KindSymbol && (strings.HasSuffix(ff[i].Name, "."+suffix) || strings.HasSuffix(ff[i].Name, "::"+suffix)) {
				f = &ff[i]
			}
		}
		if f == nil {
			t.Errorf("no symbol ending %s", suffix)
			continue
		}
		if got := f.PropString(facts.PropDoc); got != w {
			t.Errorf("%s: doc = %q, want %q", suffix, got, w)
		}
		if _, has := f.Prop(facts.PropDoc); w == "" && has {
			t.Errorf("%s: an undocumented declaration must carry no doc prop", suffix)
		}
	}
}

// A C++ declaration's doc is the comment above it or above the template holding it:
// a Doxygen block, where @brief opens the description and other tags end it, or a
// run of line comments. A plain block, a licence, a tool directive and a comment
// trailing code on its line are none.
func TestCppDoc_Declarations(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/shop.hpp": `// Copyright 2026 Someone.
class Licensed {
 public:
  int a;
};

/// Places and cancels orders.
///
/// More detail.
template <class T>
class OrderService {
 public:
  /**
   * @brief Places an order
   * and charges the card.
   * @param id the order
   */
  void place(int id);

  // Cancels an order.
  void cancel(int id);

  // NOLINT(runtime/int)
  void directive();

  /* not a doc */
  void plain();

  int max; ///< trails max
  int step;
};

//! What can be bought.
enum Item { kRed };

/** Money in cents. */
using Cents = long;
`,
		"src/shop.cpp": `#include "shop.hpp"

/// Sums two prices.
int add(int a, int b) { return a + b; }

/// Separated.

int loose() { return 0; }
`,
	})
	docBySuffix(t, ff, map[string]string{
		"Licensed":                "",
		"OrderService":            "Places and cancels orders.",
		"OrderService::place":     "Places an order and charges the card.",
		"OrderService::cancel":    "Cancels an order.",
		"OrderService::directive": "",
		"OrderService::plain":     "",
		"OrderService::step":      "",
		"Item":                    "What can be bought.",
		"Cents":                   "Money in cents.",
		"add":                     "Sums two prices.",
		"loose":                   "",
	})
}

// A definition's own doc wins over its prototype's; a prototype's doc stands in for
// a definition that has none.
func TestCppDoc_PrototypeAndDefinition(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/calc.h": `/// Adds two numbers.
int add(int a, int b);

/// Subtracts, from the header.
int sub(int a, int b);
`,
		"src/calc.c": `#include "calc.h"

int add(int a, int b) { return a + b; }

/// Subtracts, from the definition.
int sub(int a, int b) { return a - b; }
`,
		// The header sorts first here, so it is the fact the definition merges into.
		"src/a_mul.h": `/// Multiplies, from the header.
int mul(int a, int b);
`,
		"src/z_mul.c": `/// Multiplies, from the definition.
int mul(int a, int b) { return a * b; }
`,
	})
	docBySuffix(t, ff, map[string]string{
		"add": "Adds two numbers.",
		"sub": "Subtracts, from the definition.",
		"mul": "Multiplies, from the definition.",
	})
}
