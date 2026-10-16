package javaextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Java declaration's doc is its Javadoc up to the first tag, read above its
// annotations.
func TestDoc_Javadoc(t *testing.T) {
	repo := t.TempDir()
	rel := "src/main/java/com/shop/OrderService.java"
	src := `package com.shop;

/**
 * Places and cancels orders.
 * @author someone
 */
@Service
public class OrderService {
    /**
     * Places an order and charges the card.
     *
     * @param id the order
     */
    @Transactional
    public void place(String id) {}

    // Cancels an order that has not shipped.
    public void cancel(String id) {}

    public void list() {}
}
`
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ff, err := New().Extract(context.Background(), repo, []string{rel})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			got[facts.ShortName(f.Name)] = f.PropString(facts.PropDoc)
		}
	}
	for name, doc := range map[string]string{
		"OrderService": "Places and cancels orders.",
		"place":        "Places an order and charges the card.",
		"cancel":       "Cancels an order that has not shipped.",
		"list":         "",
	} {
		if got[name] != doc {
			t.Errorf("%s doc = %q, want %q (have %v)", name, got[name], doc, got)
		}
	}
}

// A package's doc is the Javadoc of its package-info.java, read above the
// annotations on the declaration. A licence there is no description.
func TestDoc_PackageInfo(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/shop/package-info.java": "/*\n * Licensed under the Apache License.\n */\n\n/**\n * Orders and payments.\n *\n * More detail.\n * @since 1.0\n */\n@NonNullApi\npackage shop;\n",
		"src/shop/Order.java":        "package shop;\n\n/** Not the package's doc. */\npublic class Order {}\n",
		"src/lic/package-info.java":  "/** Copyright 2026 Someone. */\npackage lic;\n",
		"src/bare/A.java":            "/** Only a class doc. */\npackage bare;\npublic class A {}\n",
	})
	for dir, want := range map[string]string{
		"src/shop": "Orders and payments.",
		"src/lic":  "",
		"src/bare": "",
	} {
		var mod *facts.Fact
		for i := range ff {
			if ff[i].Kind == facts.KindModule && ff[i].Name == dir {
				mod = &ff[i]
			}
		}
		if mod == nil {
			t.Errorf("no module fact %s", dir)
			continue
		}
		if got := mod.PropString(facts.PropDoc); got != want {
			t.Errorf("%s: doc = %q, want %q", dir, got, want)
		}
		if _, has := mod.Prop(facts.PropDoc); want == "" && has {
			t.Errorf("%s: an undocumented package must carry no doc prop", dir)
		}
	}
}
