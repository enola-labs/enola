package jvmsrc

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
)

// topLevelDeclRe matches a top-level declaration of any of the four JVM languages:
// a line that starts at column 0 with optional annotations and modifiers, then a
// declaration keyword. Members are indented, so column 0 is what tells a top-level
// name from a member one; the rest of the line is read by declaredName.
var topLevelDeclRe = regexp.MustCompile(`^(?:@[\w.:]+(?:\([^)]*\))?\s+)*` +
	`(?:(?:public|private|internal|protected|abstract|final|open|sealed|data|enum|annotation|inline|value|` +
	`expect|actual|static|strictfp|const|external|tailrec|operator|infix|suspend|lateinit|non-sealed|case|implicit|lazy)\s+)*` +
	`(class|interface|object|typealias|record|@interface|enum|trait|fun|val|var|def|type)\s+(.*)$`)

// identRe is a JVM identifier at the start of a string.
var identRe = regexp.MustCompile(`^[A-Za-z_$][\w$]*`)

// declaredName returns the name a top-level declaration line declares, or "" when
// the line is not one. A function or property declared with a receiver
// (`fun String.toUrl()`, `val <T> List<T>.head`) is named by the part after the
// receiver, which is the name an import writes.
func declaredName(line string) string {
	m := topLevelDeclRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	kw, rest := m[1], m[2]
	if kw == "fun" && strings.HasPrefix(rest, "interface ") {
		rest = strings.TrimPrefix(rest, "interface ")
		kw = "interface"
	}
	switch kw {
	case "fun", "val", "var", "def":
		rest = stripGenerics(rest)
		if end := strings.IndexAny(rest, "(:= {"); end >= 0 {
			rest = rest[:end]
		}
		if dot := strings.LastIndexByte(rest, '.'); dot >= 0 {
			rest = rest[dot+1:]
		}
	}
	return identRe.FindString(strings.TrimPrefix(strings.TrimSpace(rest), "`"))
}

// stripGenerics removes every <…> group, nested ones included, so the receiver of
// `<T> List<Map<K, V>>.first` reads as `List.first`.
func stripGenerics(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// BuildDeclaredNames maps "<package>.<Name>", for every top-level name declared in
// a package split across more than one non-test directory, to the directory that
// declares it. An import names a type, not a directory, and a package split across
// a library's shared and platform source sets and its testing-support module has a
// preferred directory that declares only some of its names: `import okhttp3.Headers`
// belongs where Headers is declared, whatever the package prefers.
//
// A name declared in several directories (an `expect` and its `actual`s) maps to
// the one preferDir ranks first. Test source sets are left out: production code
// never imports from them, and test code keeps the package-level answer. Packages
// declared in one directory need no entry, so only their files are read in full.
func BuildDeclaredNames(repoPath string, files []string, dirs map[string][]string) map[string]string {
	split := make(map[string]map[string]bool) // dir -> packages split across it
	for pkg, ds := range dirs {
		var prod []string
		for _, d := range ds {
			if !isTestSource(d) {
				prod = append(prod, d)
			}
		}
		if len(prod) < 2 {
			continue
		}
		for _, d := range prod {
			if split[d] == nil {
				split[d] = make(map[string]bool)
			}
			split[d][pkg] = true
		}
	}
	if len(split) == 0 {
		return nil
	}
	declarers := make(map[string][]string)
	seen := make(map[string]bool)
	for _, relFile := range files {
		if !isJVMSource(relFile) {
			continue
		}
		dir := factpath.Dir(relFile)
		if split[dir] == nil {
			continue
		}
		abs := filepath.Join(repoPath, relFile)
		pkg, ok := readPackage(abs)
		if !ok || !split[dir][pkg] {
			continue
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(src), "\n") {
			name := declaredName(strings.TrimRight(line, "\r"))
			if name == "" {
				continue
			}
			key := pkg + "." + name
			if seen[key+"\x00"+dir] {
				continue
			}
			seen[key+"\x00"+dir] = true
			declarers[key] = append(declarers[key], dir)
		}
	}
	names := make(map[string]string, len(declarers))
	for key, ds := range declarers {
		sort.Slice(ds, func(i, j int) bool { return preferDir(ds[i], ds[j]) })
		names[key] = ds[0]
	}
	return names
}

// ResolveImportNamed is ResolveImport with the names from BuildDeclaredNames: an
// import of a name declared in a split package resolves into the directory that
// declares the name rather than the package's preferred one.
func ResolveImportNamed(importPath string, index, names map[string]string) (string, bool) {
	if GeneratedImport(importPath, index) {
		return "", false
	}
	segs := strings.Split(strings.TrimSuffix(importPath, ".*"), ".")
	for i := len(segs); i > 0; i-- {
		pkg := strings.Join(segs[:i], ".")
		dir, ok := index[pkg]
		if !ok {
			continue
		}
		rest := segs[i:]
		if len(rest) == 0 {
			return dir, true
		}
		if d, ok := names[pkg+"."+rest[0]]; ok {
			dir = d
		}
		return dir + "/" + strings.Join(rest, "/"), true
	}
	return "", false
}

// DeclaringDir returns the directory that declares the name an import of a split
// package names (see BuildDeclaredNames), and false when the import names no
// package of the index, names the package alone, or names something no directory
// of a split package declares.
func DeclaringDir(importPath string, index, names map[string]string) (string, bool) {
	if len(names) == 0 {
		return "", false
	}
	p := strings.TrimSuffix(importPath, ".*")
	if _, ok := index[p]; ok {
		return "", false // the package itself
	}
	segs := strings.Split(p, ".")
	for i := len(segs) - 1; i > 0; i-- {
		pkg := strings.Join(segs[:i], ".")
		if _, ok := index[pkg]; !ok {
			continue
		}
		d, ok := names[pkg+"."+segs[i]]
		return d, ok
	}
	return "", false
}
