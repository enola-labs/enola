package jvmsrc

import (
	"strings"
	"testing"
)

// A package declared by a library's shared and platform source sets and by a
// testing-support module: the package-only answer is a production directory, never
// the support module, and the shared set ranks above a platform one.
func TestPackageDirPrefersProductionOverTestSupport(t *testing.T) {
	root, files := writeRepo(t, map[string]string{
		"lib-testing-support/src/main/kotlin/com/acme/FakeDns.kt": "package com.acme\n\nclass FakeDns\n",
		"lib/src/jvmMain/kotlin/com/acme/Platform.kt":             "package com.acme\n\nactual object Platform\n",
		"lib/src/commonJvm/kotlin/com/acme/Client.kt":             "package com.acme\n\nclass Client\n",
	})
	idx := BuildPackageIndex(root, files)
	if got, want := idx["com.acme"], "lib/src/commonJvm/kotlin/com/acme"; got != want {
		t.Errorf("com.acme -> %q, want %q", got, want)
	}
}

// An import of a name in a split package binds to the directory that declares the
// name: a type, an object member path, a top-level function. A name declared in
// several (expect and actual) binds to the preferred declarer.
func TestResolveImportBindsTheDeclaringDirectory(t *testing.T) {
	root, files := writeRepo(t, map[string]string{
		"lib-testing-support/src/main/kotlin/com/acme/FakeDns.kt": "package com.acme\n\nclass FakeDns\n",
		"lib/src/jvmMain/kotlin/com/acme/Platform.jvm.kt":         "package com.acme\n\nactual object Platform\n",
		"lib/src/androidMain/kotlin/com/acme/Platform.android.kt": "package com.acme\n\nactual object Platform\n",
		"lib/src/commonJvm/kotlin/com/acme/Platform.kt":           "package com.acme\n\nexpect object Platform\n",
		"lib/src/commonJvm/kotlin/com/acme/Client.kt": "@file:JvmName(\"Clients\")\n\npackage com.acme\n\n" +
			"public class Client {\n  class Builder\n  companion object {\n    fun String.toClient() = Client()\n  }\n}\n" +
			"internal fun <T> List<T>.firstClient(): T = first()\n",
		"lib/src/commonJvm/kotlin/com/acme/Headers.java": "package com.acme;\n\npublic final class Headers {}\n",
		"lib/src/test/kotlin/com/acme/TestOnly.kt":       "package com.acme\n\nclass TestOnly\n",
	})
	dirs := BuildPackageDirs(root, files)
	idx := IndexOf(dirs)
	names := BuildDeclaredNames(root, files, dirs)
	common := "lib/src/commonJvm/kotlin/com/acme"
	for imp, want := range map[string]string{
		"com.acme.FakeDns":                   "lib-testing-support/src/main/kotlin/com/acme/FakeDns",
		"com.acme.Client":                    common + "/Client",
		"com.acme.Client.Builder":            common + "/Client/Builder",
		"com.acme.Client.Companion.toClient": common + "/Client/Companion/toClient",
		"com.acme.firstClient":               common + "/firstClient",
		"com.acme.Headers":                   common + "/Headers",
		"com.acme.Platform":                  common + "/Platform",
		"com.acme.Unknown":                   common + "/Unknown",
		"com.acme.*":                         common,
	} {
		if got, ok := ResolveImportNamed(imp, idx, names); !ok || got != want {
			t.Errorf("ResolveImportNamed(%q) = (%q, %v), want %q", imp, got, ok, want)
		}
	}
	if d, ok := DeclaringDir("com.acme.FakeDns", idx, names); !ok || d != "lib-testing-support/src/main/kotlin/com/acme" {
		t.Errorf("DeclaringDir(com.acme.FakeDns) = (%q, %v)", d, ok)
	}
	if _, ok := names["com.acme.TestOnly"]; ok {
		t.Errorf("a name only a test source set declares must not bind production imports")
	}
}

func TestTopLevelNamesReadsPackageAndTopLevelDeclarations(t *testing.T) {
	pkg, names := TopLevelNames([]byte("package com.ex.lib\n\n@Retention(AnnotationRetention.RUNTIME)\nannotation class SpecScope\n\nclass Service {\n  annotation class Nested\n}\nfun String.helper() = 1\n"))
	if pkg != "com.ex.lib" {
		t.Errorf("pkg = %q", pkg)
	}
	if got := strings.Join(names, ","); got != "SpecScope,Service,helper" {
		t.Errorf("names = %s", got)
	}
}
