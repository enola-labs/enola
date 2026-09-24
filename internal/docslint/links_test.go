package docslint

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestLinksResolve is the check that would have caught four dead references sitting
// in the tree at once: an anchor that missed a heading gaining a package name, two
// paths whose files had been moved or renamed, and — the interesting one — a link to
// "#conventional-routing-produces-nothing" whose heading now reads "Conventional
// routing, when the registration can be read". The extractor had learned to do the
// thing the sentence said it could not, and only the anchor recorded that the claim
// had inverted.
func TestLinksResolve(t *testing.T) {
	docs := corpus(t)
	anchors := map[string]map[string]bool{}
	for _, d := range docs {
		anchors[d.Path] = d.Anchors()
	}

	var checked int
	for _, d := range docs {
		dir := path.Dir(d.Path)
		for _, l := range d.Links() {
			if strings.HasPrefix(l.Target, "http://") ||
				strings.HasPrefix(l.Target, "https://") ||
				strings.HasPrefix(l.Target, "mailto:") {
				continue
			}
			checked++
			target, frag, _ := strings.Cut(l.Target, "#")
			frag, err := url.PathUnescape(frag)
			if err != nil {
				t.Errorf("%s:%d: link %q has an unparseable fragment: %v", d.Path, l.Line, l.Target, err)
				continue
			}

			resolved := d.Path
			if target != "" {
				resolved = path.Clean(path.Join(dir, target))
				// A link that climbs out of the repository is checked BEFORE it is
				// resolved on disk, because on disk is exactly where it lies. A
				// `../../../enola-enterprise/...` link resolved on the author's
				// machine, where that repository is a sibling checkout, and failed in
				// CI where only this one exists — which is the same thing every reader
				// of a public repository experiences. Escaping the root is the defect;
				// whether the target happens to be present locally is not evidence.
				if resolved == ".." || strings.HasPrefix(resolved, "../") {
					t.Errorf("%s:%d: links to %q, which climbs out of the repository. "+
						"It can only resolve for someone who has the neighbouring checkout, "+
						"so it is broken for every other reader — link to a public URL, or "+
						"describe the thing instead of linking to it.", d.Path, l.Line, l.Target)
					continue
				}
				if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(resolved))); err != nil {
					t.Errorf("%s:%d: links to %q, which does not exist", d.Path, l.Line, l.Target)
					continue
				}
			}
			if frag == "" || !strings.HasSuffix(resolved, ".md") {
				continue
			}
			if a, ok := anchors[resolved]; !ok {
				t.Errorf("%s:%d: links into %q, which is outside the checked corpus", d.Path, l.Line, resolved)
			} else if !a[frag] {
				t.Errorf("%s:%d: links to %q, but %s has no heading with that anchor "+
					"— a heading was renamed and the link kept the old wording",
					d.Path, l.Line, l.Target, resolved)
			}
		}
	}
	if checked < 250 {
		t.Errorf("only %d relative links found; the corpus or the link regex changed", checked)
	}
}

// repoPathRe matches a backticked path into this repository. Anchored on the top
// level directories that exist, so `pkg/analyzer` (the Dart SDK's, quoted in
// docs/extraction/dart.md as an example of someone else's layout) is never mistaken
// for a claim about this tree.
var repoPathRe = regexp.MustCompile("`((?:internal|pkg|cmd|examples|docs|\\.github|\\.githooks)/[A-Za-z0-9_./-]+)`")

// foreignPaths are backticked paths that deliberately do not exist HERE, because
// they name a file in a different repository: the one enola is installed into, or a
// third-party project quoted as an example. Listing them is the cost of checking the
// rest, and it keeps each exception visible rather than encoding a blanket rule like
// "ignore anything under internal/" that would quietly stop checking the real ones.
var foreignPaths = map[string]string{
	"internal/auth":                              "the worked example for --target and --max-spillover, in docs/GATING.md, ARCHITECTURE.md and docs/CLI.md",
	"internal/auth/handler.go":                   "the same worked example, in ARCHITECTURE.md",
	"internal/session":                           "the second package in the --max-spillover example in docs/CLI.md",
	"internal/engine.cacheVersion":               "a Go selector written as a path, naming the cacheVersion constant, in docs/CLI.md",
	"pkg/analyzer":                               "the Dart SDK's own package, quoted in docs/extraction/dart.md",
	"pkg/front_end":                              "the Dart SDK's own package, quoted in docs/extraction/dart.md and ARCHITECTURE.md",
	".github/instructions":                       "Copilot's instructions directory in the repository enola installs INTO — written by pkg/install, never present here",
	".github/instructions/enola.instructions.md": "the file pkg/install writes into that repository, listed in the install target table",
}

// TestBacktickedPathsExist checks the references that are not links. Most of the
// paths in these pages are markdown links and covered above, but the prose also
// names files in running text — and those rot exactly as fast. `cmd/enola/hook.go`
// was named in two pages for as long as the file had lived in pkg/command.
func TestBacktickedPathsExist(t *testing.T) {
	var checked int
	for _, d := range corpus(t) {
		for _, m := range repoPathRe.FindAllStringSubmatchIndex(d.Prose, -1) {
			p := d.Prose[m[2]:m[3]]
			if _, ok := foreignPaths[strings.TrimSuffix(p, "/")]; ok {
				continue
			}
			checked++
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(p))); err != nil {
				line := strings.Count(d.Prose[:m[0]], "\n") + 1
				t.Errorf("%s:%d: names `%s`, which does not exist — either the path moved, "+
					"or it lives in another repository and belongs in foreignPaths with a reason",
					d.Path, line, p)
			}
		}
	}
	if checked < 80 {
		t.Errorf("only %d backticked repo paths found; the regex or the pages changed", checked)
	}
}

// TestStaleForeignPaths is the converse. A waived path that now EXISTS is no longer
// foreign, and leaving it waived means the real one stops being checked.
func TestStaleForeignPaths(t *testing.T) {
	for p, why := range foreignPaths {
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(p))); err == nil {
			t.Errorf("foreignPaths waives %q (%s), but that path now exists — "+
				"drop the waiver so the reference is checked", p, why)
		}
	}
}

// guardedPaths are the executable files that decide behaviour from a path, where a
// rename is silent in a way even a failing test would not reveal: the guard simply
// stops firing.
var guardedPaths = []string{".githooks/pre-push"}

// pathInGuard matches a repo path inside a shell pattern, tolerating the backslash
// escaping a regex alternation needs (`pkg/command/hook\.go`).
var pathInGuard = regexp.MustCompile(`(?:internal|pkg|cmd)/[A-Za-z0-9_./\\-]*[A-Za-z0-9_/]`)

// TestGuardPathsExist extends the same check to code that is not documentation.
//
// .githooks/pre-push runs the agent-hook end-to-end test only when the push touches
// the installer, and it decides that by matching changed files against a list of
// paths. Two of them named cmd/enola/, which is where those files used to live. The
// hook stayed green, the test simply never ran again — and its own comment says
// nothing else can catch a hook that never fires. That is the failure this test
// exists for: a path claim in a file that has no other reader.
func TestGuardPathsExist(t *testing.T) {
	for _, g := range guardedPaths {
		body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(g)))
		if err != nil {
			t.Fatalf("reading %s: %v", g, err)
		}
		found := 0
		for _, m := range pathInGuard.FindAllString(string(body), -1) {
			p := strings.ReplaceAll(m, `\`, "")
			if strings.HasSuffix(p, "/") || !strings.Contains(p, ".") {
				// A directory prefix like pkg/install/ — check the directory.
				p = strings.TrimSuffix(p, "/")
			}
			found++
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(p))); err != nil {
				t.Errorf("%s matches on %q, which does not exist — the guard silently stopped firing", g, p)
			}
		}
		if found == 0 {
			t.Errorf("%s: no guarded paths found; the file or the regex changed", g)
		}
	}
}

func corpus(t *testing.T) []Doc {
	t.Helper()
	docs, err := Corpus()
	if err != nil {
		t.Fatalf("reading the documentation corpus: %v", err)
	}
	if len(docs) < 20 {
		t.Fatalf("found %d markdown pages, expected the full docs tree", len(docs))
	}
	return docs
}
