// Package estateref keeps the names of the private repositories this tree is
// measured against out of the tree itself.
//
// Almost every comment here that carries a number earned it on a real
// repository, and naming that repository beside the number is the natural way
// to write it down. It is also how the name travels: this folder is the one
// that can reach a contribution, so a comment citing a measurement by the
// repository it was measured on carries that repository's name with it.
//
// Removing the names downstream, on the branch a contribution is prepared
// from, does not hold. One refresh from this folder put nineteen of them back,
// and they had to be removed by hand a second time. The check therefore runs
// where the references are written, in the normal test gate, so a refresh that
// reintroduces one fails immediately and names it.
//
// The names are not written down here. They are read at test time from a file
// outside this tree, kept where a carve cannot reach it, because a denylist is
// an inventory: eleven names and a person in one machine-readable slice hand a
// reader in a single file what sixty scattered comments only hinted at. Where
// that file is absent — which is what this tree looks like once it has been
// split out on its own — there is nothing to check, and the check skips and
// says why. Everything else in this package is exercised either way.
//
// What is neutralised is the identity and never the evidence. "3,392 of 3,732
// routes unmatched" is why the code around it is shaped as it is; the name of
// the repository it was measured on is not. A comment that drops the number to
// lose the name is worse than the name.
package estateref

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/workspace"
)

// ErrTermsAbsent reports that the denylist is not on disk. It is a state and
// not a failure: the file is deliberately outside the tree this package
// checks, so a copy of the tree standing on its own has nothing to check
// against. Callers skip on it; anything else is a real error.
var ErrTermsAbsent = errors.New("estate terms file absent")

// maxFileSize bounds what is read. Nothing a human wrote a repository name
// into is four megabytes; a file that size is generated output, and a local
// snapshot dropped inside this tree carries a fact stream that runs to tens of
// megabytes. Reading those turns the check into the slow, noisy one everybody
// learns to ignore.
const maxFileSize = 4 << 20

// skipNames are trees no human writes a comment in. .git holds every past
// version of every neutralised line, so scanning it would report history that
// cannot be edited. A snapshot directory holds this tool's own output about
// other repositories, which names them by design.
var skipNames = map[string]bool{".git": true, "node_modules": true}

// Term is one name and the rules for finding it.
type Term struct {
	// Text is the name as written, and what a hit reports.
	Text string

	// Word marks a name that is also an ordinary English word, which then
	// matches only where it is not part of a longer one. Coined names carry no
	// such restriction and match wherever they are written, including inside a
	// longer identifier — that is where a refresh puts them back, and a rule
	// that stopped at word boundaries would read as though it had decided
	// those spellings were fine.
	Word bool

	// Except are path prefixes that do not report this name, for lines this
	// tree inherits verbatim from pristine upstream. Forbidding one of those
	// would fail the check on code we have no standing to change; omitting the
	// name entirely would leave the tree unguarded everywhere else.
	Except []string
}

// Hit is one occurrence: enough to open the file at the line and see the term.
type Hit struct {
	File string // slash-separated, relative to the scanned root
	Line int    // 1-based, or 0 when the name is in the path rather than the content
	Term string // the entry from the denylist that matched
	Text string // the line it matched on, trimmed; the name itself for a path
}

func (h Hit) String() string {
	if h.Line == 0 {
		return fmt.Sprintf("%s: %q — in the path", h.File, h.Term)
	}
	return fmt.Sprintf("%s:%d: %q — %s", h.File, h.Line, h.Term, h.Text)
}

// LoadTerms reads the denylist. The format is one name per line, then
// modifiers: "word" for a name that is also an ordinary English word, and
// "except <prefix>" for a path prefix that does not report it. Blank lines and
// lines opening with # are commentary.
func LoadTerms(path string) ([]Term, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", path, ErrTermsAbsent)
	}
	if err != nil {
		return nil, err
	}

	var terms []Term
	for number, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		term := Term{Text: fields[0]}
		for i := 1; i < len(fields); i++ {
			switch fields[i] {
			case "word":
				term.Word = true
			case "except":
				i++
				if i == len(fields) {
					return nil, fmt.Errorf("%s:%d: except names no path prefix", path, number+1)
				}
				term.Except = append(term.Except, fields[i])
			default:
				return nil, fmt.Errorf("%s:%d: unknown modifier %q", path, number+1, fields[i])
			}
		}
		terms = append(terms, term)
	}
	if len(terms) == 0 {
		return nil, fmt.Errorf("%s: no names", path)
	}
	return terms, nil
}

// Scan reports every occurrence of a term under root, in walk order.
//
// It reads every regular text file rather than a list of extensions, because a
// name reaches a contribution the same way from a fixture, a manifest or a
// document as it does from a comment. Files carrying a NUL byte are not text
// and are skipped. Names are checked as well as content: a fixture directory
// or a golden file called after the repository it was taken from carries the
// name exactly as a comment does, and renaming a file is what a refresh does
// most readily.
func Scan(root string, terms []Term) ([]Hit, error) {
	var hits []Hit
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "." {
			return nil
		}
		// Each name is checked once, where it is written: a directory at its
		// own entry and a file by its base name. Checking whole paths instead
		// would report a directory again for every file beneath it.
		if entry.IsDir() {
			if skipDir(entry.Name()) || workspace.IsLinkedWorktree(path) {
				return fs.SkipDir
			}
			hits = append(hits, nameHits(terms, relative, entry.Name())...)
			return nil
		}
		hits = append(hits, nameHits(terms, relative, entry.Name())...)
		// A symlink carries two names and no content: its own, checked above,
		// and its target, which git stores as the blob and a carve reproduces
		// verbatim. Reading the target rather than following it keeps the walk
		// on this tree and still names a link pointing outside it.
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			for _, found := range lineMatches(terms, target) {
				if exempt(found.term, relative) {
					continue
				}
				hits = append(hits, Hit{
					File: relative,
					Line: 1,
					Term: found.term.Text,
					Text: target,
				})
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFileSize {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.IndexByte(string(content), 0) >= 0 {
			return nil
		}
		for number, line := range strings.Split(string(content), "\n") {
			for _, found := range lineMatches(terms, line) {
				if exempt(found.term, relative) {
					continue
				}
				hits = append(hits, Hit{
					File: relative,
					Line: number + 1,
					Term: found.term.Text,
					Text: strings.TrimSpace(line),
				})
			}
		}
		return nil
	})
	return hits, err
}

// skipDir also drops a snapshot directory, whose name is either .enola or
// .enola- and a cluster label.
func skipDir(name string) bool {
	return skipNames[name] || name == ".enola" || strings.HasPrefix(name, ".enola-")
}

func nameHits(terms []Term, relative, name string) []Hit {
	var hits []Hit
	for _, found := range lineMatches(terms, name) {
		if exempt(found.term, relative) {
			continue
		}
		hits = append(hits, Hit{File: relative, Term: found.term.Text, Text: name})
	}
	return hits
}

func exempt(term Term, relative string) bool {
	for _, prefix := range term.Except {
		if strings.HasPrefix(relative, prefix) {
			return true
		}
	}
	return false
}

type match struct {
	at   int
	term Term
}

// lineMatches reports every term written anywhere in one line, left to right.
func lineMatches(terms []Term, line string) []match {
	folded, origin := fold(line)
	var found []match
	for _, term := range terms {
		needle, _ := fold(term.Text)
		if needle == "" {
			continue
		}
		for from := 0; from+len(needle) <= len(folded); {
			offset := strings.Index(folded[from:], needle)
			if offset < 0 {
				break
			}
			at := from + offset
			from = at + 1
			start, end := origin[at], origin[at+len(needle)-1]
			if term.Word && (letterAt(line, start-1) || letterAt(line, end+1)) {
				continue
			}
			found = append(found, match{at: start, term: term})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].at != found[j].at {
			return found[i].at < found[j].at
		}
		return found[i].term.Text < found[j].term.Text
	})
	return found
}

// fold lowercases and drops the separators identifiers are built from, so one
// entry catches every spelling a name is written in: a name shouted, a name in
// camel case inside a longer identifier, and a name whose hyphen became an
// underscore are all the same name as the one on the list. origin maps each
// folded byte back to where it came from, which is what the whole-word rule for
// ordinary English words is decided on: a separator the fold removed is still a
// boundary in the line the reader sees.
func fold(s string) (string, []int) {
	var folded strings.Builder
	folded.Grow(len(s))
	origin := make([]int, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' || c == '_' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		folded.WriteByte(c)
		origin = append(origin, i)
	}
	return folded.String(), origin
}

func letterAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i] | 0x20
	return c >= 'a' && c <= 'z'
}
