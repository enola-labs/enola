package javaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Java import of a Kotlin type in a package split across a library's source sets
// and its testing-support module binds to the directory declaring the type; a
// wildcard import of the package binds to a production directory, never the
// support module.
func TestJavaImportOfSplitKotlinPackageBindsTheDeclaringDirectory(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"lib-testing-support/src/main/kotlin/com/acme/FakeDns.kt": "package com.acme\n\nclass FakeDns\n",
		"lib/src/commonJvm/kotlin/com/acme/Client.kt":             "package com.acme\n\nclass Client\n",
		"lib/src/jvmMain/kotlin/com/acme/Platform.kt":             "package com.acme\n\nactual object Platform\n",
		"app/src/main/java/com/app/Main.java": "package com.app;\n\n" +
			"import com.acme.Client;\nimport com.acme.FakeDns;\nimport com.acme.*;\n\npublic class Main {}\n",
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		imp, _ := f.PropAny("import").(string)
		if wc, _ := f.PropAny("wildcard").(bool); wc {
			imp += ".*"
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports {
				got[imp] = r.Target
			}
		}
	}
	for imp, want := range map[string]string{
		"com.acme.Client":  "lib/src/commonJvm/kotlin/com/acme",
		"com.acme.FakeDns": "lib-testing-support/src/main/kotlin/com/acme",
		"com.acme.*":       "lib/src/commonJvm/kotlin/com/acme",
	} {
		if got[imp] != want {
			t.Errorf("import %s -> %q, want %q (all %v)", imp, got[imp], want, got)
		}
	}
}
