package kotlinextractor

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func kmpRepoSymbols(t *testing.T, files map[string]string) map[string]facts.Fact {
	t.Helper()
	dir, rel := writeKotlinRepo(t, files)
	var kt []string
	for _, f := range rel {
		if isKotlinFile(f) {
			kt = append(kt, f)
		}
	}
	all, err := New().Extract(context.Background(), dir, kt)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]facts.Fact{}
	for _, f := range all {
		if f.Kind == facts.KindSymbol {
			out[f.Name] = f
		}
	}
	return out
}

// TestKMPExportsStampFrameworkAndFacade: a top-level function of an Apple source
// set in a module building a framework carries the framework and its file's
// facade; a JVM or Android source set, a member, and a module building no
// framework carry neither.
func TestKMPExportsStampFrameworkAndFacade(t *testing.T) {
	syms := kmpRepoSymbols(t, map[string]string{
		"shared/build.gradle.kts": `kotlin {
    listOf(iosArm64(), iosSimulatorArm64()).forEach {
        it.binaries.framework {
            baseName = "shared"
        }
    }
}`,
		"shared/src/iosMain/kotlin/app/main.ios.kt":         "package app\n\nfun MainViewController(): Any = 1\n\nclass Holder { fun member() {} }\n",
		"shared/src/commonMain/kotlin/app/Greeting.kt":      "package app\n\nfun greet() {}\n",
		"shared/src/androidMain/kotlin/app/main.android.kt": "package app\n\nfun MainActivityHost() {}\n",
		"server/build.gradle.kts":                           `plugins { kotlin("jvm") }`,
		"server/src/main/kotlin/srv/Main.kt":                "package srv\n\nfun main() {}\n",
	})
	check := func(name, framework, facade string) {
		t.Helper()
		f, ok := syms[name]
		if !ok {
			t.Fatalf("no symbol %s", name)
		}
		if got := f.PropString(facts.PropKMPFramework); got != framework {
			t.Errorf("%s %s = %q, want %q", name, facts.PropKMPFramework, got, framework)
		}
		if got := f.PropString(facts.PropKotlinFacade); got != facade {
			t.Errorf("%s %s = %q, want %q", name, facts.PropKotlinFacade, got, facade)
		}
	}
	check("shared/src/iosMain/kotlin/app.MainViewController", "shared", "Main_iosKt")
	check("shared/src/commonMain/kotlin/app.greet", "shared", "GreetingKt")
	check("shared/src/iosMain/kotlin/app.Holder.member", "", "")
	check("shared/src/androidMain/kotlin/app.MainActivityHost", "", "")
	check("server/src/main/kotlin/srv.main", "", "")
}

// TestKMPFrameworkDefaultsToModuleName: a framework with no baseName is named
// after its Gradle module.
func TestKMPFrameworkDefaultsToModuleName(t *testing.T) {
	syms := kmpRepoSymbols(t, map[string]string{
		"mobile/core/build.gradle.kts":            "kotlin { iosArm64 { binaries { framework() } } }",
		"mobile/core/src/iosMain/kotlin/c/Api.kt": "package c\n\nfun start() {}\n",
	})
	if got := syms["mobile/core/src/iosMain/kotlin/c.start"].PropString(facts.PropKMPFramework); got != "core" {
		t.Fatalf("framework = %q, want core", got)
	}
}
