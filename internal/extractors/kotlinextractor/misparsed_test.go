package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Two annotations on a top-level declaration with another after it send the grammar
// down its script branch. An interface, a class and an abstract class are recovered
// with their members; an object keeps its declaration and gets its annotations back.
func TestKotlinMisparsedDeclarationsAreRecovered(t *testing.T) {
	ff := extractAST(t, `
package com.acme.di

import dagger.Module
import dagger.hilt.InstallIn
import dagger.hilt.EntryPoint

@EntryPoint
@InstallIn(SingletonComponent::class)
interface WorkerEntryPoint {
    fun factory(): Factory
}

private const val NAME = "x"

@Module
@InstallIn(SingletonComponent::class)
abstract class DataModule {
    fun bind(): Int = helper()
}

@Tag("slow")
@Suppress("unused")
class Screen {
    fun draw() {}
}

@Module
@InstallIn(SingletonComponent::class)
object NetworkModule {
    fun provide(): Int = 1
}

fun helper(): Int = 1
`, true)
	for name, kind := range map[string]string{
		"WorkerEntryPoint":         facts.SymbolInterface,
		"WorkerEntryPoint.factory": facts.SymbolMethod,
		"DataModule":               facts.SymbolClass,
		"DataModule.bind":          facts.SymbolMethod,
		"Screen":                   facts.SymbolClass,
		"Screen.draw":              facts.SymbolMethod,
		"NetworkModule":            facts.SymbolClass,
	} {
		f, ok := findFactSuffix(ff, "pkg."+name)
		if !ok {
			t.Errorf("%s: not declared", name)
			continue
		}
		if got := f.PropString("symbol_kind"); got != kind {
			t.Errorf("%s symbol_kind = %q, want %q", name, got, kind)
		}
	}
	if f, ok := findFactSuffix(ff, "pkg.DataModule"); ok {
		if f.Props["abstract"] != true || f.Props["di_module"] != true || f.Props["framework_registered"] != true {
			t.Errorf("DataModule props = %v, want abstract, di_module and framework_registered", f.Props)
		}
	}
	if f, ok := findFactSuffix(ff, "pkg.NetworkModule"); ok && f.Props["di_module"] != true {
		t.Errorf("NetworkModule props = %v, want its @Module back (di_module)", f.Props)
	}
	if f, ok := findFactSuffix(ff, "pkg.DataModule.bind"); ok && !f.HasRelation(facts.RelCalls, "pkg.helper") {
		t.Errorf("DataModule.bind relations = %v, want a call to helper", f.Relations)
	}
}

func findFactSuffix(ff []facts.Fact, suffix string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && len(f.Name) >= len(suffix) && f.Name[len(f.Name)-len(suffix):] == suffix {
			return f, true
		}
	}
	return facts.Fact{}, false
}

// Body-less classes under stacked annotations chain into one expression, each the
// right operand of the one before; every class in the chain is declared, with its
// own annotations.
func TestKotlinMisparsedChainIsRecovered(t *testing.T) {
	ff := extractKotlinRepo(t, map[string]string{"src/main/kotlin/pkg/Graal.kt": `package pkg

@TargetClass(BouncyCastlePlatform::class)
@Delete
class TargetBouncy

@TargetClass(ConscryptPlatform::class)
@Delete
class TargetConscrypt

@TargetClass(Platform::class)
class TargetPlatform {
    fun run() {}
}

class BouncyCastlePlatform
class ConscryptPlatform
class Platform
`})
	for _, name := range []string{"src/main/kotlin/pkg.TargetBouncy", "src/main/kotlin/pkg.TargetConscrypt", "src/main/kotlin/pkg.TargetPlatform", "src/main/kotlin/pkg.TargetPlatform.run"} {
		if _, ok := findFact(ff, name); !ok {
			t.Errorf("%s: not declared", name)
		}
	}
	bouncy, _ := findFact(ff, "src/main/kotlin/pkg.TargetBouncy")
	conscrypt, _ := findFact(ff, "src/main/kotlin/pkg.TargetConscrypt")
	if !bouncy.HasRelation(facts.RelNames, "src/main/kotlin/pkg.BouncyCastlePlatform") {
		t.Errorf("TargetBouncy relations = %v, want names BouncyCastlePlatform", bouncy.Relations)
	}
	if !conscrypt.HasRelation(facts.RelNames, "src/main/kotlin/pkg.ConscryptPlatform") || conscrypt.HasRelation(facts.RelNames, "src/main/kotlin/pkg.BouncyCastlePlatform") {
		t.Errorf("TargetConscrypt relations = %v, want its own annotation's ConscryptPlatform only", conscrypt.Relations)
	}
}
