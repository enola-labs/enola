package javaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A package directory names the build module it compiles into.
func TestModulesNameTheirBuildModule(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"billing/src/main/java/com/shop/billing/Invoice.java": "package com.shop.billing;\n\npublic class Invoice {}\n",
	})
	for _, f := range ff {
		if f.Kind == facts.KindModule && f.Name == "billing/src/main/java/com/shop/billing" {
			if got := f.PropAny("jvm_module"); got != "billing" {
				t.Fatalf("jvm_module = %v, want billing", got)
			}
			return
		}
	}
	t.Fatal("no module fact for the package directory")
}
