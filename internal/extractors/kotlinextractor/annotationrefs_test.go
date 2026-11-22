package kotlinextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// An annotation on a function, a class, an object or a split-off annotation stack
// names its type where the repository declares it: imported, from the file's own
// package, or used through a qualified path.
func TestKotlinDeclarationAnnotationNamesItsType(t *testing.T) {
	const ui = "core/ui/src/main/kotlin/com/acme/ui"
	const app = "app/src/main/kotlin/com/acme/app"
	ff := extractKotlinRepo(t, map[string]string{
		ui + "/Previews.kt": `package com.acme.ui

annotation class DevicePreviews

annotation class Marker(val name: String)

@DevicePreviews
fun LocalPreview() {}
`,
		app + "/Screen.kt": `package com.acme.app

import com.acme.ui.DevicePreviews
import com.acme.ui.Marker

annotation class Feature

@DevicePreviews
@Composable
fun ScreenPreview() {}

@Marker("x")
class Screen {
    @Feature
    fun render() {}
}

@Feature
object Registry

@Feature
@Marker("y")
object Split

fun after() {}
`,
	})
	for _, c := range []struct{ sym, want string }{
		{app + ".ScreenPreview", ui + ".DevicePreviews"},
		{ui + ".LocalPreview", ui + ".DevicePreviews"},
		{app + ".Screen", ui + ".Marker"},
		{app + ".Screen.render", app + ".Feature"},
		{app + ".Registry", app + ".Feature"},
		{app + ".Split", app + ".Feature"},
		{app + ".Split", ui + ".Marker"},
	} {
		if f := ktSym(t, ff, c.sym); !f.HasRelation(facts.RelNames, c.want) {
			t.Errorf("%s relations = %v, want names %s", c.sym, f.Relations, c.want)
		}
	}
}

// An annotation from outside the repository, or one Kotlin imports by default,
// records nothing, also where the repository declares a type of the same name.
func TestKotlinDeclarationAnnotationDoesNotBindFrameworkNames(t *testing.T) {
	const dir = "lib/src/main/kotlin/com/acme"
	ff := extractKotlinRepo(t, map[string]string{
		dir + "/Names.kt": `package com.acme

class Preview
class Deprecated
class JvmStatic
class Volatile
`,
		dir + "/Use.kt": `package com.acme

import androidx.compose.ui.tooling.preview.Preview

@Preview
@Deprecated("old")
fun Old() {}

object Holder {
    @JvmStatic
    fun make() {}

    @Volatile
    var flag = false
}
`,
	})
	for _, f := range ff {
		if f.Kind != facts.KindSymbol || f.File != dir+"/Use.kt" {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames && strings.HasPrefix(r.Target, dir+".") {
				t.Errorf("%s names %s, a framework annotation bound to a repository type", f.Name, r.Target)
			}
		}
	}
}
