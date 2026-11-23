package kotlinextractor

import (
	"context"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// ktorModuleRefs returns the calls targets of the file_ref facts a config file
// produced, keyed by the config file.
func ktorModuleRefs(t *testing.T, files map[string]string) map[string][]string {
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
	out := map[string][]string{}
	for _, f := range all {
		if f.Kind != facts.KindFileRef {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				out[f.File] = append(out[f.File], r.Target)
			}
		}
		sort.Strings(out[f.File])
	}
	return out
}

const ktorModulesSrc = `package com.example.server

import io.ktor.server.application.*

fun Application.routesModule() {}

fun Application.unlisted() {}
`

// TestKtorConfigModules_YAML: a module EngineMain loads by name from
// application.yaml is referenced by that file, through its file facade class.
func TestKtorConfigModules_YAML(t *testing.T) {
	got := ktorModuleRefs(t, map[string]string{
		"server/src/main/kotlin/com/example/server/RoutesModule.kt": ktorModulesSrc,
		"server/src/main/resources/application.yaml": `ktor:
  deployment:
    port: 8080
  application:
    modules:
      - com.example.server.RoutesModuleKt.routesModule
      - com.example.server.OtherKt.routesModule
      - com.example.server.RoutesModuleKt.missing
`,
	})
	want := []string{"server/src/main/kotlin/com/example/server.routesModule"}
	if g := got["server/src/main/resources/application.yaml"]; len(g) != 1 || g[0] != want[0] {
		t.Fatalf("refs = %v, want %v (only the entry whose facade and function exist)", got, want)
	}
}

// TestKtorConfigModules_HOCON reads the same key from application.conf, nested
// and dotted, with comments and quoted entries, and a @file:JvmName facade.
func TestKtorConfigModules_HOCON(t *testing.T) {
	got := ktorModuleRefs(t, map[string]string{
		"src/main/kotlin/com/example/server/RoutesModule.kt": ktorModulesSrc,
		"src/main/kotlin/com/example/server/Plugins.kt": `@file:JvmName("PluginSetup")
package com.example.server

fun Application.plugins() {}
`,
		"src/main/kotlin/com/example/server/Main.kt": `package com.example.server

object Modules {
    fun Application.objectModule() {}
}
`,
		"src/main/resources/application.conf": `# comment
ktor {
    deployment { port = 8080 }
    application {
        // the modules
        modules = [ com.example.server.RoutesModuleKt.routesModule,
                    "com.example.server.PluginSetup.plugins",
                    com.example.server.PluginsKt.plugins ]
    }
}
ktor.application.modules = [ com.example.server.Modules.objectModule ]
other { modules = [ com.example.server.RoutesModuleKt.unlisted ] }
`,
	})
	want := []string{
		"src/main/kotlin/com/example/server.Modules.objectModule",
		"src/main/kotlin/com/example/server.plugins",
		"src/main/kotlin/com/example/server.routesModule",
	}
	g := got["src/main/resources/application.conf"]
	if len(g) != len(want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("refs = %v, want %v", g, want)
		}
	}
}

// TestKtorConfigModules_NoConfigNoRef: without a config naming it, a module
// function stays unreferenced; a test resources config roots nothing.
func TestKtorConfigModules_NoConfigNoRef(t *testing.T) {
	got := ktorModuleRefs(t, map[string]string{
		"src/main/kotlin/com/example/server/RoutesModule.kt": ktorModulesSrc,
		"src/test/kotlin/com/example/server/T.kt":            "package com.example.server\n",
		"src/test/resources/application.conf":                "ktor.application.modules = [ com.example.server.RoutesModuleKt.routesModule ]\n",
	})
	if len(got) != 0 {
		t.Fatalf("refs = %v, want none", got)
	}
}
