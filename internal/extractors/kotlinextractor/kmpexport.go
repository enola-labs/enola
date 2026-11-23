package kotlinextractor

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/jvmsrc"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Kotlin Multiplatform functions exported to Swift.
//
// A Gradle module that builds an Apple framework (`binaries.framework { baseName
// = "shared" }`) exports the top-level functions of its Apple source sets to
// Swift as static members of each file's facade class: `main.ios.kt` declares
// `fun MainViewController()`, Swift imports `shared` and calls
// `Main_iosKt.MainViewController()`. The walker cannot see the Swift side, so it
// stamps each such function with the framework and the facade, and the kmp-swift
// binder binds a Swift call through the framework the calling file imports.

var (
	// kmpFrameworkRe finds an Apple framework a build declares: a
	// `binaries.framework`, an `XCFramework(...)`, or a CocoaPods `framework {}`.
	kmpFrameworkRe = regexp.MustCompile(`\bframework\s*[({]|\bXCFramework\s*\(`)
	// kmpBaseNameRe reads the framework's name.
	kmpBaseNameRe = regexp.MustCompile(`\bbaseName\s*(?:=\s*|\.set\(\s*)"([A-Za-z_][A-Za-z0-9_]*)"`)
)

// kmpFrameworks returns, per Gradle module directory, the Apple framework names
// its build file declares. A framework with no baseName is named after the
// module's directory.
func kmpFrameworks(repoPath string, units map[string]bool) map[string][]string {
	out := map[string][]string{}
	for unit := range units {
		if unit == "" {
			continue
		}
		var src []byte
		for _, name := range []string{"build.gradle.kts", "build.gradle"} {
			if b, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(unit), name)); err == nil {
				src = b
				break
			}
		}
		if src == nil || !kmpFrameworkRe.Match(src) {
			continue
		}
		seen := map[string]bool{}
		for _, m := range kmpBaseNameRe.FindAllSubmatch(src, -1) {
			if n := string(m[1]); !seen[n] {
				seen[n] = true
				out[unit] = append(out[unit], n)
			}
		}
		if len(out[unit]) == 0 && unit != "." {
			out[unit] = []string{path.Base(unit)}
		}
	}
	return out
}

// nonAppleSourceSet reports whether a Kotlin source set never compiles for an
// Apple target: the JVM, Android, JS, Wasm, desktop and the other native hosts.
func nonAppleSourceSet(set string) bool {
	for _, p := range []string{"android", "jvm", "js", "wasm", "desktop", "web", "linux", "mingw", "windows"} {
		if strings.HasPrefix(set, p) {
			return true
		}
	}
	return false
}

// sourceSetOf returns the source set a file sits in (`iosMain` for
// `shared/src/iosMain/kotlin/...`), or "".
func sourceSetOf(rel string) string {
	segs := strings.Split(rel, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "src" {
			return segs[i+1]
		}
	}
	return ""
}

// stampKMPExports marks each top-level function of an Apple source set in a
// module that builds a framework with the framework and its file facade.
func stampKMPExports(repoPath string, all []facts.Fact) {
	units := map[string]bool{}
	for i := range all {
		if all[i].Kind == facts.KindSymbol {
			units[jvmsrc.BuildModule(factpath.Dir(all[i].File))] = true
		}
	}
	frameworks := kmpFrameworks(repoPath, units)
	if len(frameworks) == 0 {
		return
	}
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol || f.PropString("symbol_kind") != facts.SymbolFunc || !isKotlinFile(f.File) {
			continue
		}
		dir := factpath.Dir(f.File)
		names := frameworks[jvmsrc.BuildModule(dir)]
		if len(names) == 0 || !strings.HasPrefix(f.Name, dir+".") || strings.Contains(f.Name[len(dir)+1:], ".") {
			continue // not top-level
		}
		set := sourceSetOf(f.File)
		if set == "" || nonAppleSourceSet(set) || jvmsrc.IsTestSource(f.File) || facts.IsTestPath(f.File) {
			continue
		}
		f.SetProp(facts.PropKMPFramework, strings.Join(names, ","))
		f.SetProp(facts.PropKotlinFacade, kotlinFacadeName(f.File))
	}
}
