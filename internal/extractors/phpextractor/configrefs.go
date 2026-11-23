package phpextractor

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Files that are not PHP register PHP classes by name, and nothing in PHP names
// them: a PHPStan .neon file lists its extensions as services, and `artisan
// db:seed --class=` in a composer script or a shell script runs a seeder. Each
// such file gets one file reference naming the classes it registers.
//
// Only a class the repository declares binds, by its full name. A class written
// inside a quoted string (a PHPStan ignored-error pattern) is a mention, not a
// registration, and is left alone.

// neonClassValue matches a .neon line whose value is a class name: a service list
// entry, a `class:` or `factory:` key, an extension, a `@Service` reference. An
// argument list or a `::method` after the name is allowed.
var neonClassValue = regexp.MustCompile(`^\s*(?:-\s*)?(?:[\w.-]+\s*:\s*)?@?\\?([A-Za-z_]\w*(?:\\[A-Za-z_]\w*)+)(?:::\w+)?(?:\(.*)?\s*$`)

// seederClass matches the seeder an `artisan db:seed` command line runs.
var seederClass = regexp.MustCompile(`--class[= ]["']?([A-Za-z_\\][\w\\]*)`)

func isNeonFile(rel string) bool {
	l := strings.ToLower(rel)
	return strings.HasSuffix(l, ".neon") || strings.HasSuffix(l, ".neon.dist")
}

func isScriptFile(rel string) bool {
	switch base := path.Base(rel); {
	case base == "composer.json", base == "Makefile":
		return true
	default:
		return strings.HasSuffix(strings.ToLower(base), ".sh")
	}
}

// extractConfigClassRefs reads the .neon and script files among files and returns a
// file reference for each that registers a class declared names.
func extractConfigClassRefs(repoPath string, files []string, declared map[string]bool) []facts.Fact {
	var out []facts.Fact
	for _, rel := range files {
		neon, script := isNeonFile(rel), isScriptFile(rel)
		if (!neon && !script) || facts.IsTestPath(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		found := map[string]bool{}
		for _, ln := range strings.Split(string(src), "\n") {
			switch {
			case neon:
				ln, _, _ = strings.Cut(ln, "#")
				if m := neonClassValue.FindStringSubmatch(ln); m != nil && declared[m[1]] {
					found[m[1]] = true
				}
			case strings.Contains(ln, "db:seed"):
				for _, m := range seederClass.FindAllStringSubmatch(ln, -1) {
					if class := seederName(m[1], declared); class != "" {
						found[class] = true
					}
				}
			}
		}
		if len(found) == 0 {
			continue
		}
		classes := make([]string, 0, len(found))
		for c := range found {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		rels := make([]facts.Relation, 0, len(classes))
		for _, c := range classes {
			rels = append(rels, facts.Relation{Kind: facts.RelNames, Target: c})
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindFileRef,
			Name:      rel,
			File:      rel,
			Line:      1,
			Props:     map[string]any{"language": "php"},
			Relations: rels,
		})
	}
	return out
}

// seederName resolves a --class= value as `artisan db:seed` does: a name with no
// namespace is a class in Database\Seeders, and before Laravel 8 a global class.
// A JSON string writes each backslash twice.
func seederName(raw string, declared map[string]bool) string {
	raw = strings.TrimLeft(strings.ReplaceAll(raw, `\\`, `\`), `\`)
	if !strings.Contains(raw, `\`) && declared[`Database\Seeders\`+raw] {
		return `Database\Seeders\` + raw
	}
	if declared[raw] {
		return raw
	}
	return ""
}
