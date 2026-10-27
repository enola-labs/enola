package command

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/locate"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
	"github.com/enola-labs/enola/pkg/history"
)

// Locate is `enola locate`: the symbol at a place in source, and what became of a
// remembered one. The locate tool, without a session.
//
// It is the call a script makes to key what it keeps about code on a fact id rather than a
// line number: stamp a finding with the symbol it points into, map a fix's diff to the
// symbols it changed, and later ask whether those symbols still hold the same code. --at
// answers against a recorded revision instead of the working tree, so a finding made at an
// old commit is stamped against the code as it was, without checking that commit out.
func (r *Runner) Locate(ctx context.Context, args []string) {
	os.Exit(r.runLocate(ctx, args, os.Stdin, os.Stdout, os.Stderr))
}

func (r *Runner) runLocate(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	usageError := check.StatusUsageError.ExitCode()
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintf(stderr, r.name()+" locate: "+format+"\n", args...)
	}

	fs := flag.NewFlagSet("locate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		diffPath   = fs.String("diff", "", "a unified diff file, or - for stdin: report the symbols its changed lines fall in")
		side       = fs.String("side", locate.SideNew, "which side of the diff the graph is of: new (the tree with the change) or old (its parent)")
		stampsPath = fs.String("stamps", "", "a JSONL file of stamps ({id, body_hash, name}), or - for stdin: report each as current, rewritten, moved or gone")
		at         = fs.String("at", "", "answer against this recorded revision instead of the working tree (a snapshot id, git commit, HEAD~N, @<seq> or ref)")
		asJSON     = fs.Bool("json", false, "emit the result as JSON")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr,
			"Usage: "+r.name()+" locate [flags] [<file:line>...] [repo_path|config_path]\n\n"+
				"Map places in source to the symbols the graph holds there, and remembered\n"+
				"symbols to where they are now.\n\n"+
				"A location is file:line or file:start-end, with the file as the graph names\n"+
				"it or any path ending in it. Each resolves to the innermost symbol holding it:\n"+
				"its fact id (the id in facts.jsonl) and its body_hash, the version of its code.\n"+
				"A language whose extractor measures no spans gets the nearest declaration\n"+
				"above, and says so.\n\n"+
				"Exit codes:\n"+
				"  0  the lookup ran\n"+
				"  2  the command could not run\n\nFlags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "locate")
	}
	if err := fs.Parse(args); err != nil {
		return usageError
	}
	locs, repoArg := splitLocationsAndRepo(fs.Args())
	if len(locs) == 0 && *diffPath == "" && *stampsPath == "" {
		fs.Usage()
		return usageError
	}
	if *diffPath == "-" && *stampsPath == "-" {
		say("--diff and --stamps cannot both read stdin")
		return usageError
	}
	read := func(p string) ([]byte, error) {
		if p == "-" {
			return io.ReadAll(stdin)
		}
		return os.ReadFile(p)
	}
	var diffText []byte
	if *diffPath != "" {
		var err error
		if diffText, err = read(*diffPath); err != nil {
			say("reading the diff: %v", err)
			return usageError
		}
	}
	var stamps []locate.Stamp
	if *stampsPath != "" {
		data, err := read(*stampsPath)
		if err != nil {
			say("reading the stamps: %v", err)
			return usageError
		}
		if stamps, err = parseStamps(data); err != nil {
			say("%v", err)
			return usageError
		}
	}

	ff, err := r.locateFacts(ctx, repoArg, *at, say)
	if err != nil {
		say("%v", err)
		return usageError
	}
	ix := locate.NewIndex(ff)

	var out struct {
		Locations []locate.LocationResult `json:"locations,omitempty"`
		Diff      *locate.DiffResult      `json:"diff,omitempty"`
		Stamps    []locate.CheckResult    `json:"stamps,omitempty"`
	}
	if len(locs) > 0 {
		out.Locations = ix.Locations(locs)
	}
	if diffText != nil {
		d, err := ix.Diff(string(diffText), *side)
		if err != nil {
			say("%v", err)
			return usageError
		}
		out.Diff = &d
	}
	if len(stamps) > 0 {
		out.Stamps = ix.Check(stamps)
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			say("encoding the result: %v", err)
			return usageError
		}
		return 0
	}
	var w strings.Builder
	for _, l := range out.Locations {
		writeLocation(&w, l)
	}
	if out.Diff != nil {
		writeLocateDiff(&w, *out.Diff)
	}
	for _, c := range out.Stamps {
		writeStampCheck(&w, c)
	}
	_, _ = io.WriteString(stdout, w.String())
	return 0
}

// locateFacts returns the facts a lookup answers against: a recorded revision's with
// at, otherwise the current snapshot's, taken first when the one on disk is stale.
func (r *Runner) locateFacts(ctx context.Context, repoArg, at string, say func(string, ...any)) ([]facts.Fact, error) {
	if at != "" {
		repoPath, cfg := r.logTarget(repoArg)
		root, err := history.Root(repoPath, cfg.History.Dir)
		if err != nil {
			return nil, fmt.Errorf("cannot locate the history: %w", err)
		}
		entries, err := history.Read(root)
		if err != nil {
			if errors.Is(err, history.ErrNoHistory) {
				return nil, fmt.Errorf("--at needs a recorded history, and %s has none", repoPath)
			}
			return nil, err
		}
		e, err := history.Resolve(history.SortedByTime(entries), at)
		if err != nil {
			return nil, err
		}
		snap, err := history.Load(root, e)
		if err != nil {
			return nil, fmt.Errorf("revision %s: %w", e.Short(), err)
		}
		say("answering against revision %s (%s)", e.Short(), shortDate(e.At))
		return snap.Facts, nil
	}

	tgt := r.resolveTarget(repoArg)
	say("%s", tgt.configNote)
	tgt.engine.SetPersistCache(false)
	generatedAt, stale := snapshotIsCurrent(tgt.engine, tgt.repoPaths)
	if stale == "" {
		if err := bootstrap.RestoreSnapshot(tgt.engine, tgt.repoPaths); err != nil {
			stale = "the snapshot on disk could not be read: " + err.Error()
		}
	}
	if stale == "" {
		say("reading the snapshot written %s, which matches the working tree", generatedAt)
	} else {
		say("taking a snapshot for this run, %s", stale)
		for i, repoPath := range tgt.repoPaths {
			tgt.engine.SetDeferLinking(i < len(tgt.repoPaths)-1)
			if _, err := tgt.engine.GenerateSnapshot(ctx, repoPath, i > 0); err != nil {
				return nil, fmt.Errorf("snapshot generation failed for %s: %w", repoPath, err)
			}
		}
	}
	return tgt.engine.Store().FactsRef(), nil
}

// splitLocationsAndRepo separates the locations from an optional trailing repository or
// config path. A location has a `:line` suffix; the repository argument does not.
func splitLocationsAndRepo(args []string) ([]string, string) {
	if n := len(args); n > 0 {
		if _, _, _, err := locate.ParseLocation(args[n-1]); err != nil {
			return args[:n-1], args[n-1]
		}
	}
	return args, ""
}

func parseStamps(data []byte) ([]locate.Stamp, error) {
	var out []locate.Stamp
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var st locate.Stamp
		if err := json.Unmarshal([]byte(line), &st); err != nil || st.ID == "" {
			return nil, fmt.Errorf("stamps line %d is not {\"id\": ..., \"body_hash\": ..., \"name\": ...}", n)
		}
		out = append(out, st)
	}
	return out, sc.Err()
}

func symbolLine(s locate.Symbol) string {
	at := fmt.Sprintf("%s:%d", s.File, s.Line)
	if s.EndLine > 0 {
		at += fmt.Sprintf("-%d", s.EndLine)
	}
	line := fmt.Sprintf("%s  (%s)  id %s", s.Name, at, s.ID)
	if s.BodyHash != "" {
		line += "  body " + s.BodyHash
	}
	return line
}

func writeLocation(w *strings.Builder, l locate.LocationResult) {
	switch {
	case l.Error != "":
		fmt.Fprintf(w, "%s  %s\n", l.Location, l.Error)
	case l.Match == locate.MatchNoFile && len(l.Candidates) > 0:
		fmt.Fprintf(w, "%s  matches %d files: %s\n", l.Location, len(l.Candidates), strings.Join(l.Candidates, ", "))
	case l.Match == locate.MatchNoFile:
		fmt.Fprintf(w, "%s  no symbols in the graph for this file\n", l.Location)
	case len(l.Symbols) == 0:
		fmt.Fprintf(w, "%s  outside every symbol\n", l.Location)
	default:
		fmt.Fprintf(w, "%s  %s\n", l.Location, l.Match)
		for _, s := range l.Symbols {
			fmt.Fprintf(w, "    %s\n", symbolLine(s))
		}
	}
}

func writeLocateDiff(w *strings.Builder, d locate.DiffResult) {
	fmt.Fprintf(w, "diff (%s side): %d symbol%s\n", d.Side, d.Symbols, pluralS(d.Symbols))
	for _, f := range d.Files {
		switch f.Match {
		case locate.MatchNoFile:
			fmt.Fprintf(w, "  %s  no symbols in the graph for this file\n", f.Path)
			continue
		case locate.MatchNearest:
			fmt.Fprintf(w, "  %s  (no spans: nearest declaration above)\n", f.File)
		default:
			fmt.Fprintf(w, "  %s\n", f.File)
		}
		for _, s := range f.Symbols {
			fmt.Fprintf(w, "    %3d  %s\n", s.Lines, symbolLine(s.Symbol))
		}
		if f.Outside > 0 {
			fmt.Fprintf(w, "    %3d  outside every symbol\n", f.Outside)
		}
	}
}

func writeStampCheck(w *strings.Builder, c locate.CheckResult) {
	name := c.Stamp.Name
	if name == "" {
		name = c.Stamp.ID
	}
	fmt.Fprintf(w, "%-9s  %s\n", c.Status, name)
	if c.Symbol != nil {
		fmt.Fprintf(w, "           now %s\n", symbolLine(*c.Symbol))
	}
	for _, s := range c.Candidates {
		fmt.Fprintf(w, "           same name: %s\n", symbolLine(s))
	}
}
