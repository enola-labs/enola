// Package providerspec is the declared vocabulary of fact providers: what a
// `providers:` entry in the engine config may say, and whether a given list of
// entries is well formed.
//
// It is the declaration only. Running a provider, and the engines the built-in
// ones read through, live in internal/providers. The split is what lets the
// config validate a provider list at load without importing the code that runs
// one, in the same way internal/clientspec declares the client vocabulary the
// extractors read.
package providerspec

import (
	"fmt"
	"sort"
	"strings"
)

// Spec is one configured fact source: the census name its facts are
// stamped with, the command (argv) the seam runs, and the version the
// operator expects the installed build to report. An entry with no command
// names a provider the binary carries itself (see BuiltInNames), run
// in-process under the same validation and census. A reported version that
// disagrees with the expected one is a skip, not a merge — facts from a build
// the config did not pin would drift the graph for reasons no code change made.
type Spec struct {
	Name            string   `yaml:"name"`
	Command         []string `yaml:"command"`
	ExpectedVersion string   `yaml:"expected_version"`
	// Files declares how the provider's output partitions. Empty means the
	// provider reads the whole tree on every run. FilesPerFile means every
	// fact it emits names the file it came from and nothing else influenced
	// it, so the seam may hand it only the files whose content has no cache
	// entry and reuse the rest; a provider that reads across files must not
	// declare it, because the cache would then serve facts computed against
	// a tree that has since changed.
	Files string `yaml:"files,omitempty"`
	// Extensions names the file suffixes a per-file provider reads, so the
	// seam keys and lists only those; required with FilesPerFile.
	Extensions []string `yaml:"extensions,omitempty"`
}

// FilesPerFile is the one value Spec.Files accepts.
const FilesPerFile = "per-file"

// builtIns names the providers the binary carries itself, which an entry may
// name without a command. A name, not an implementation: internal/providers
// holds one runner per name, and its tests fail if the two sets differ.
var builtIns = []string{"rubydex"}

// BuiltInNames lists the providers a config may name without a command.
func BuiltInNames() []string {
	names := append([]string(nil), builtIns...)
	sort.Strings(names)
	return names
}

func isBuiltIn(name string) bool {
	for _, b := range builtIns {
		if b == name {
			return true
		}
	}
	return false
}

// Validate checks the configured provider list's shape at config load, with
// the same contract every other vocabulary gets: named errors, nothing silent.
func Validate(providers []Spec) error {
	seen := map[string]bool{}
	for i, p := range providers {
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("providers[%d]: missing name", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("providers[%d]: name %q is declared twice", i, p.Name)
		}
		seen[p.Name] = true
		if p.Files != "" && p.Files != FilesPerFile {
			return fmt.Errorf("providers[%d] (%s): files must be %q or absent, got %q", i, p.Name, FilesPerFile, p.Files)
		}
		if p.Files == FilesPerFile && len(p.Extensions) == 0 {
			return fmt.Errorf("providers[%d] (%s): files: %s needs the extensions the provider reads", i, p.Name, FilesPerFile)
		}
		if p.Files == "" && len(p.Extensions) > 0 {
			return fmt.Errorf("providers[%d] (%s): extensions only mean something with files: %s", i, p.Name, FilesPerFile)
		}
		if len(p.Command) == 0 {
			if !isBuiltIn(p.Name) {
				return fmt.Errorf("providers[%d] (%s): missing command, and no built-in provider has that name (built-ins: %s)", i, p.Name, strings.Join(BuiltInNames(), ", "))
			}
			if p.Files != "" {
				return fmt.Errorf("providers[%d] (%s): a built-in provider decides its own caching; files: is for commands", i, p.Name)
			}
			continue
		}
		if strings.TrimSpace(p.Command[0]) == "" {
			return fmt.Errorf("providers[%d] (%s): missing command", i, p.Name)
		}
	}
	return nil
}
