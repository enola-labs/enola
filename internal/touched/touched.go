// Package touched answers which symbols a run of commits changed, without a snapshot per
// commit: it keeps one scratch tree in step with the history and extracts only the files
// each commit changed.
//
// It reads the repository and writes nothing into it.
package touched

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/locate"
)

// Extract returns the facts for a few files of the tree at root.
type Extract func(ctx context.Context, root string, files []string) ([]facts.Fact, error)

// Options selects the commits and what counts towards the ranking.
type Options struct {
	Rev   string // the history to walk; HEAD when empty
	Since string // git's --since
	Until string // git's --until
	// Subject keeps the commits whose subject matches it.
	Subject *regexp.Regexp
	// MaxSymbols leaves out of the ranking a commit changing more callables than this: a
	// sweep across a hundred functions says little about any of them. 0 keeps every commit.
	MaxSymbols int
	// Declarations credits a symbol that is part of a declaration to the declaration.
	Declarations bool
}

// Commit is one commit and the symbols it changed, on its new side.
type Commit struct {
	SHA     string `json:"sha"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
	// Ranked says the commit counts towards the ranking.
	Ranked  bool                `json:"ranked"`
	Files   []string            `json:"files,omitempty"`
	Symbols []locate.DiffSymbol `json:"symbols,omitempty"`
	// Outside counts changed lines in files with symbols that no symbol holds.
	Outside int `json:"outside,omitempty"`
}

// Ranked is one callable and the ranked commits that changed it.
type Ranked struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	SymbolKind string   `json:"symbol_kind"`
	File       string   `json:"file"`
	Count      int      `json:"count"`
	Commits    []string `json:"commits,omitempty"`
}

// Result is the walk, oldest commit first.
type Result struct {
	Commits []Commit `json:"commits"`
	// RankedCommits counts the commits the ranking is over.
	RankedCommits int `json:"ranked_commits"`
	// Symbols counts the ranking's rows, before anything limits how many are returned.
	Symbols int      `json:"symbols"`
	Ranking []Ranked `json:"ranking,omitempty"`
}

func callable(kind string) bool {
	return kind == facts.SymbolFunc || kind == facts.SymbolMethod || kind == facts.SymbolGetter
}

// Run walks the selected commits, oldest first.
func Run(ctx context.Context, repo string, o Options, extract Extract) (*Result, error) {
	commits, err := selectCommits(ctx, repo, o)
	if err != nil {
		return nil, err
	}
	res := &Result{Commits: commits}
	if len(commits) == 0 {
		return res, nil
	}

	scratch, err := os.MkdirTemp("", "enola-touched-")
	if err != nil {
		return nil, fmt.Errorf("creating a scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	// Named after the repository, as a snapshot's tree is: an extractor can read the
	// directory's name, and a temp name would differ on every run.
	tree := filepath.Join(scratch, filepath.Base(repo))
	if err := os.MkdirAll(tree, 0o755); err != nil {
		return nil, fmt.Errorf("creating the scratch tree: %w", err)
	}

	at := ""
	for i := range res.Commits {
		c := &res.Commits[i]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := syncTree(ctx, repo, tree, at, c.SHA); err != nil {
			return nil, fmt.Errorf("commit %s: %w", c.SHA, err)
		}
		at = c.SHA
		if err := attribute(ctx, repo, tree, c, o, extract); err != nil {
			return nil, fmt.Errorf("commit %s: %w", c.SHA, err)
		}
	}
	rank(res)
	return res, nil
}

func attribute(ctx context.Context, repo, tree string, c *Commit, o Options, extract Extract) error {
	names, err := git(ctx, repo, "diff-tree", "--root", "--no-commit-id", "--no-renames", "--name-only", "-r", "-z", c.SHA)
	if err != nil {
		return err
	}
	for _, p := range bytes.Split(names, []byte{0}) {
		if len(p) > 0 {
			c.Files = append(c.Files, string(p))
		}
	}
	sort.Strings(c.Files)
	ff, err := extract(ctx, tree, c.Files)
	if err != nil {
		return err
	}
	text, err := git(ctx, repo, "-c", "core.quotepath=off", "diff-tree", "--root", "-p", "-U0", "--no-renames",
		"--no-commit-id", "--no-color", "--no-ext-diff", "--no-textconv", c.SHA)
	if err != nil {
		return err
	}
	d, err := locate.NewIndex(ff).Diff(string(text), locate.SideNew)
	if err != nil {
		return err
	}
	var declaration map[string]locate.Symbol
	if o.Declarations {
		declaration = declarations(ff)
	}
	callables := map[string]bool{}
	for _, f := range d.Files {
		// The index holds only this commit's files, so a path with no symbols can only
		// resolve to another file by its suffix, and that is not the same file.
		if f.File != f.Path {
			continue
		}
		c.Outside += f.Outside
		first := len(c.Symbols)
		for _, s := range f.Symbols {
			if parent, ok := declaration[s.MemberOf+"\x00"+s.File]; ok {
				s.Symbol = parent
			}
			merged := false
			for i := first; i < len(c.Symbols); i++ {
				if c.Symbols[i].ID == s.ID && c.Symbols[i].Line == s.Line {
					c.Symbols[i].Lines += s.Lines
					merged = true
					break
				}
			}
			if !merged {
				c.Symbols = append(c.Symbols, s)
			}
			if callable(s.SymbolKind) {
				callables[s.ID] = true
			}
		}
	}
	c.Ranked = len(callables) > 0 && (o.MaxSymbols == 0 || len(callables) <= o.MaxSymbols)
	return nil
}

// declarations indexes the symbols other symbols can be part of, by name and file.
func declarations(ff []facts.Fact) map[string]locate.Symbol {
	parents := map[string]bool{}
	for i := range ff {
		if p := ff[i].PropString(facts.PropMemberOf); p != "" {
			parents[p+"\x00"+ff[i].File] = true
		}
	}
	out := map[string]locate.Symbol{}
	for _, file := range locate.NewIndex(ff).Symbols() {
		if key := file.Name + "\x00" + file.File; parents[key] {
			out[key] = file
		}
	}
	return out
}

func rank(res *Result) {
	byID := map[string]*Ranked{}
	for _, c := range res.Commits {
		if !c.Ranked {
			continue
		}
		res.RankedCommits++
		seen := map[string]bool{}
		for _, s := range c.Symbols {
			if !callable(s.SymbolKind) || seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			r, ok := byID[s.ID]
			if !ok {
				r = &Ranked{ID: s.ID, Name: s.Name, SymbolKind: s.SymbolKind, File: s.File}
				byID[s.ID] = r
			}
			r.Count++
			r.Commits = append(r.Commits, c.SHA)
		}
	}
	for _, r := range byID {
		res.Ranking = append(res.Ranking, *r)
	}
	res.Symbols = len(res.Ranking)
	sort.Slice(res.Ranking, func(i, j int) bool {
		a, b := res.Ranking[i], res.Ranking[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.File < b.File
	})
}

func selectCommits(ctx context.Context, repo string, o Options) ([]Commit, error) {
	rev := o.Rev
	if rev == "" {
		rev = "HEAD"
	}
	args := []string{"log", "--no-merges", "--first-parent", "--format=%H%x00%cI%x00%s"}
	if o.Since != "" {
		args = append(args, "--since="+o.Since)
	}
	if o.Until != "" {
		args = append(args, "--until="+o.Until)
	}
	out, err := git(ctx, repo, append(args, rev, "--")...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := strings.SplitN(line, "\x00", 3)
		if len(p) != 3 {
			continue
		}
		if o.Subject == nil || o.Subject.MatchString(p[2]) {
			commits = append(commits, Commit{SHA: p[0], Date: p[1], Subject: p[2]})
		}
	}
	for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
		commits[i], commits[j] = commits[j], commits[i]
	}
	return commits, nil
}

// syncTree brings the tree at dir from commit `from` to commit `to`. The first call unpacks
// the whole tree; later ones write only what changed between the two.
func syncTree(ctx context.Context, repo, dir, from, to string) error {
	if from == "" {
		archive := exec.CommandContext(ctx, "git", "-C", repo, "archive", "--format=tar", to)
		extract := exec.CommandContext(ctx, "tar", "-x", "-C", dir)
		pipe, err := archive.StdoutPipe()
		if err != nil {
			return err
		}
		extract.Stdin = pipe
		if err := extract.Start(); err != nil {
			return fmt.Errorf("extracting the tree: %w", err)
		}
		if err := archive.Run(); err != nil {
			_ = extract.Wait()
			return fmt.Errorf("reading the tree: %w", err)
		}
		return extract.Wait()
	}

	raw, err := git(ctx, repo, "diff", "--raw", "-z", "--no-abbrev", "--no-renames", from, to)
	if err != nil {
		return err
	}
	changes, err := parseRaw(raw)
	if err != nil {
		return err
	}
	// Removals first: a file can be replaced by a directory of the same name.
	var writes []change
	for _, c := range changes {
		if c.oid == zeroOID {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(c.path))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if c.mode != modeSubmodule {
			writes = append(writes, c)
		}
	}
	return writeBlobs(ctx, repo, dir, writes)
}

const (
	zeroOID       = "0000000000000000000000000000000000000000"
	modeSubmodule = "160000"
	modeSymlink   = "120000"
)

type change struct{ mode, oid, path string }

// parseRaw reads `git diff --raw -z --no-abbrev --no-renames`: a metadata field and a path
// per change. A removal has the null object name on its new side.
func parseRaw(raw []byte) ([]change, error) {
	fields := bytes.Split(raw, []byte{0})
	var out []change
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(string(fields[i]))
		if len(meta) != 5 {
			return nil, fmt.Errorf("unexpected diff line %q", fields[i])
		}
		oid := meta[3]
		if strings.Trim(oid, "0") == "" {
			oid = zeroOID
		}
		out = append(out, change{mode: meta[1], oid: oid, path: string(fields[i+1])})
	}
	return out, nil
}

// writeBlobs writes the changes' contents through one `git cat-file --batch`.
func writeBlobs(ctx context.Context, repo, dir string, changes []change) error {
	if len(changes) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "--batch")
	var input strings.Builder
	for _, c := range changes {
		input.WriteString(c.oid + "\n")
	}
	cmd.Stdin = strings.NewReader(input.String())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r := bufio.NewReaderSize(stdout, 1<<16)
	for _, c := range changes {
		content, err := readBlob(r)
		if err != nil {
			_ = cmd.Wait()
			return fmt.Errorf("reading %s: %w", c.path, err)
		}
		if err := writeEntry(dir, c, content); err != nil {
			_ = cmd.Wait()
			return err
		}
	}
	return cmd.Wait()
}

func readBlob(r *bufio.Reader) ([]byte, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	f := strings.Fields(header)
	if len(f) != 3 {
		return nil, fmt.Errorf("unexpected object header %q", strings.TrimSpace(header))
	}
	size, err := strconv.Atoi(f[2])
	if err != nil {
		return nil, err
	}
	content := make([]byte, size+1) // the trailing newline cat-file adds
	if _, err := io.ReadFull(r, content); err != nil {
		return nil, err
	}
	return content[:size], nil
}

func writeEntry(dir string, c change, content []byte) error {
	path := filepath.Join(dir, filepath.FromSlash(c.path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// A directory or a symlink may stand where this file now goes.
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if c.mode == modeSymlink {
		return os.Symlink(string(content), path)
	}
	return os.WriteFile(path, content, 0o644)
}

func git(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
