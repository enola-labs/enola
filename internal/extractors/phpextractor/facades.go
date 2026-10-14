package phpextractor

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Laravel facades of the application's own.
//
// `Preferences::mark()` is a static call on a facade, a class whose only content
// names the key of a service in the container:
//
//	class Preferences extends Facade {
//	    protected static function getFacadeAccessor(): string { return 'preferences'; }
//	}
//
// and a service provider binds the key to a class:
//
//	$this->app->bind('preferences', static fn (): Preferences => new Preferences());
//	public array $singletons = ['activity' => ActivityLogger::class];
//
// The call runs that class's method. Recorded as a call on the facade, it reaches
// nothing: Firefly III makes 981 calls through its own facades, and a flow stopped at
// every one. Facades of the framework itself (Log, DB, Cache) are outside the
// repository and are left as they are.
//
// The facades and providers are found by where Laravel puts them, a path naming
// Facade or Provider, so the pass reads a few dozen files and not the repository.

var (
	phpNamespace   = regexp.MustCompile(`(?m)^\s*namespace\s+([\w\\]+)\s*;`)
	phpUse         = regexp.MustCompile(`(?m)^\s*use\s+\\?([\w\\]+)(?:\s+as\s+(\w+))?\s*;`)
	facadeClass    = regexp.MustCompile(`class\s+(\w+)\s+extends\s+\\?(?:[\w\\]*\\)?Facade\b`)
	facadeAccessor = regexp.MustCompile(`function\s+getFacadeAccessor\s*\([^)]*\)[^{]*\{\s*return\s+(?:'([^']+)'|"([^"]+)"|\\?([\w\\]+)::class)\s*;`)
	bindClass      = regexp.MustCompile(`->(?:bind|singleton|scoped)\(\s*['"]([^'"]+)['"]\s*,\s*\\?([\w\\]+)::class`)
	bindClosure    = regexp.MustCompile(`->(?:bind|singleton|scoped)\(\s*['"]([^'"]+)['"]\s*,\s*(?:static\s+)?(?:fn|function)\s*\([^)]*\)\s*(?::\s*\??\\?([\w\\]+))?[^;]*?(?:new\s+\\?([\w\\]+))?`)
	bindArrayEntry = regexp.MustCompile(`['"]([\w.\-]+)['"]\s*=>\s*\\?([\w\\]+)::class`)
)

// phpNames resolves a class name written in one file to its fully qualified name.
type phpNames struct {
	namespace string
	uses      map[string]string
}

func readPHPNames(src []byte) phpNames {
	n := phpNames{uses: map[string]string{}}
	if m := phpNamespace.FindSubmatch(src); m != nil {
		n.namespace = string(m[1])
	}
	for _, m := range phpUse.FindAllSubmatch(src, -1) {
		full := string(m[1])
		alias := string(m[2])
		if alias == "" {
			alias = full[strings.LastIndexByte(full, '\\')+1:]
		}
		n.uses[alias] = full
	}
	return n
}

func (n phpNames) qualify(name string) string {
	name = strings.TrimPrefix(name, "\\")
	head, rest, nested := strings.Cut(name, "\\")
	if full, ok := n.uses[head]; ok {
		if nested {
			return full + "\\" + rest
		}
		return full
	}
	if n.namespace == "" {
		return name
	}
	return n.namespace + "\\" + name
}

// facadeTargets maps each facade class of the repository to the class its key is
// bound to.
func facadeTargets(repoPath string, files []string) map[string]string {
	accessors := map[string]string{} // facade class -> key, or "class:" + bound class
	bindings := map[string]string{}  // key -> class
	sorted := append([]string{}, files...)
	sort.Strings(sorted)
	for _, rel := range sorted {
		if !strings.Contains(rel, "Facade") && !strings.Contains(rel, "Provider") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		names := readPHPNames(src)
		if c := facadeClass.FindSubmatch(src); c != nil {
			if a := facadeAccessor.FindSubmatch(src); a != nil {
				facade := names.qualify(string(c[1]))
				switch {
				case len(a[1]) > 0:
					accessors[facade] = string(a[1])
				case len(a[2]) > 0:
					accessors[facade] = string(a[2])
				default:
					accessors[facade] = "class:" + names.qualify(string(a[3]))
				}
			}
		}
		if !strings.Contains(rel, "Provider") {
			continue
		}
		bind := func(key, class string) {
			if class == "" {
				return
			}
			if _, seen := bindings[key]; !seen {
				bindings[key] = names.qualify(class)
			}
		}
		for _, m := range bindClass.FindAllSubmatch(src, -1) {
			bind(string(m[1]), string(m[2]))
		}
		for _, m := range bindClosure.FindAllSubmatch(src, -1) {
			class := string(m[2])
			if class == "" {
				class = string(m[3])
			}
			bind(string(m[1]), class)
		}
		for _, m := range bindArrayEntry.FindAllSubmatch(src, -1) {
			bind(string(m[1]), string(m[2]))
		}
	}
	out := map[string]string{}
	for facade, accessor := range accessors {
		if class, ok := strings.CutPrefix(accessor, "class:"); ok {
			out[facade] = class
		} else if class := bindings[accessor]; class != "" {
			out[facade] = class
		}
	}
	return out
}

// resolveFacadeCalls rewrites each static call on an application facade to the
// method of the class behind it, when that class declares the method.
func resolveFacadeCalls(allFacts []facts.Fact, targets map[string]string) {
	if len(targets) == 0 {
		return
	}
	declared := map[string]bool{}
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindSymbol {
			declared[allFacts[i].Name] = true
		}
	}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		for j := range f.Relations {
			r := &f.Relations[j]
			if r.Kind != facts.RelCalls {
				continue
			}
			facade, method, ok := strings.Cut(r.Target, "::")
			if !ok {
				continue
			}
			if class := targets[facade]; class != "" && declared[class+"::"+method] {
				r.Target = class + "::" + method
			}
		}
	}
}
