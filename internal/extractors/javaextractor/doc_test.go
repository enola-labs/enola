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
