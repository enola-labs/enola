package rubyextractor

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linescan"
)

// scheduleConfigGlobs are the files a scheduler reads class names from:
// sidekiq-cron and sidekiq-scheduler (`class: Reports::NightlyJob`), a Temporal
// schedule (`workflow: CleanupWorkflow`), at the application's config/ or a
// package's or engine's.
var scheduleConfigGlobs = []string{
	"config/*schedul*.yml", "config/*schedul*.yaml", "config/sidekiq*.yml",
	"*/*/config/*schedul*.yml", "*/*/config/*schedul*.yaml", "*/*/config/sidekiq*.yml",
}

// scheduleClassLine matches a key that names the class or workflow a scheduler
// instantiates, with the constant written bare or quoted.
var scheduleClassLine = regexp.MustCompile(`^\s*(?:class|workflow|worker|job_class):\s*['"]?((?:::)?[A-Z][A-Za-z0-9_]*(?:::[A-Z][A-Za-z0-9_]*)*)['"]?\s*(?:#.*)?$`)

// scheduleRefs returns a file reference per scheduler config naming the classes
// it runs. A job registered only there is reached by the scheduler, never by a
// call, and read as code nothing uses.
func scheduleRefs(repoPath string) []facts.Fact {
	seen := map[string]bool{}
	var files []string
	for _, g := range scheduleConfigGlobs {
		matches, _ := filepath.Glob(filepath.Join(repoPath, g))
		for _, m := range matches {
			rel, err := filepath.Rel(repoPath, m)
			if err != nil || seen[rel] {
				continue
			}
			seen[rel] = true
			files = append(files, filepath.ToSlash(rel))
		}
	}
	sort.Strings(files)
	var out []facts.Fact
	for _, rel := range files {
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		var rels []facts.Relation
		named := map[string]bool{}
		sc := linescan.New(bytes.NewReader(src))
		for sc.Scan() {
			m := scheduleClassLine.FindStringSubmatch(sc.Text())
			if m == nil || named[m[1]] {
				continue
			}
			named[m[1]] = true
			rels = append(rels, facts.Relation{Kind: facts.RelNames, Target: trimRootColons(m[1])})
		}
		if len(rels) == 0 {
			continue
		}
		out = append(out, facts.Fact{
			Kind: facts.KindFileRef, Name: rel, File: rel, Line: 1,
			Props:     map[string]any{"language": "ruby", "scheduler_config": true},
			Relations: rels,
		})
	}
	return out
}

func trimRootColons(s string) string {
	if len(s) > 2 && s[:2] == "::" {
		return s[2:]
	}
	return s
}
