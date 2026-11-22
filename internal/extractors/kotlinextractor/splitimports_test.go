package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// splitPackageRepo is a library whose root package is declared by its shared and
// platform source sets and by a testing-support module.
var splitPackageRepo = map[string]string{
	"lib-testing-support/src/main/kotlin/com/acme/FakeDns.kt": "package com.acme\n\nclass FakeDns\n",
	"lib/src/commonJvm/kotlin/com/acme/Client.kt":             "package com.acme\n\nclass Client {\n  companion object {\n    fun String.toClient() = Client()\n  }\n}\n",
	"lib/src/commonJvm/kotlin/com/acme/Platform.kt":           "package com.acme\n\nexpect object Platform\n",
	"lib/src/jvmMain/kotlin/com/acme/Platform.jvm.kt":         "package com.acme\n\nactual object Platform\n",
	"lib/src/androidMain/kotlin/com/acme/Platform.android.kt": "package com.acme\n\nactual object Platform\n",
}

// An import of a split package binds to the directory that declares the imported
// name, including a name several platform sets declare and a companion member.
func TestKotlinImportOfSplitPackageBindsTheDeclaringDirectory(t *testing.T) {
	files := map[string]string{
		"app/src/main/kotlin/com/app/Main.kt": "package com.app\n\n" +
			"import com.acme.Client\nimport com.acme.Client.Companion.toClient\nimport com.acme.Platform\nimport com.acme.FakeDns\n\n" +
			"class Main\n",
	}
	for k, v := range splitPackageRepo {
		files[k] = v
	}
	got := map[string]bool{}
	for _, f := range extractKotlinRepo(t, files) {
		if f.Kind != facts.KindDependency || f.File != "app/src/main/kotlin/com/app/Main.kt" {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports {
				got[r.Target] = true
			}
		}
	}
	for _, want := range []string{
		"lib/src/commonJvm/kotlin/com/acme/Client",
		"lib/src/commonJvm/kotlin/com/acme/Client/Companion/toClient",
		"lib/src/commonJvm/kotlin/com/acme/Platform",
		"lib-testing-support/src/main/kotlin/com/acme/FakeDns",
	} {
		if !got[want] {
			t.Errorf("no import edge to %s; got %v", want, got)
		}
	}
}
