package kotlinextractor

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func metroSymbols(t *testing.T, files map[string]string) map[string]facts.Fact {
	t.Helper()
	dir, rel := writeKotlinRepo(t, files)
	all, err := New().Extract(context.Background(), dir, rel)
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

// TestMetroContributedContainerIsRegistered: a binding container contributed to
// a scope is merged by the compiler and registered; one with no @ContributesTo is
// a DI module only; an unrelated annotation of the same name registers nothing.
func TestMetroContributedContainerIsRegistered(t *testing.T) {
	syms := metroSymbols(t, map[string]string{
		"src/main/kotlin/di/AndroidBindings.kt": `package di

import dev.zacsweers.metro.AppScope
import dev.zacsweers.metro.BindingContainer
import dev.zacsweers.metro.ContributesTo

@BindingContainer
@ContributesTo(AppScope::class)
object AndroidBindings {
    fun x() = 1
}
`,
		"src/main/kotlin/di/IosBindings.kt": `package di

import dev.zacsweers.metro.*

@BindingContainer
@ContributesTo(AppScope::class)
interface IosBindings
`,
		"src/main/kotlin/di/Listed.kt": `package di

import dev.zacsweers.metro.BindingContainer

@BindingContainer
object Listed
`,
		"src/main/kotlin/di/Other.kt": `package di

import my.own.ContributesTo
import my.own.BindingContainer

@BindingContainer
@ContributesTo(Thing::class)
class Other
`,
		"src/main/kotlin/di/AnvilModule.kt": `package di

import com.squareup.anvil.annotations.ContributesTo
import dagger.Module

@Module
@ContributesTo(AppScope::class)
interface AnvilModule
`,
	})
	check := func(name string, registered, module bool) {
		t.Helper()
		f, ok := syms[name]
		if !ok {
			t.Fatalf("no symbol %s", name)
		}
		if got, _ := f.PropAny("framework_registered").(bool); got != registered {
			t.Errorf("%s framework_registered = %v, want %v", name, got, registered)
		}
		if got, _ := f.PropAny("di_module").(bool); got != module {
			t.Errorf("%s di_module = %v, want %v", name, got, module)
		}
	}
	check("src/main/kotlin/di.AndroidBindings", true, true)
	check("src/main/kotlin/di.IosBindings", true, true)
	check("src/main/kotlin/di.Listed", false, true)
	check("src/main/kotlin/di.Other", false, false)
	check("src/main/kotlin/di.AnvilModule", true, false)
}
