package command

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/enola-labs/enola/internal/endpoint"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
)

// Endpoint answers what changing an HTTP endpoint reaches.
//
// The MCP tool has answered this since it was built, which means only an agent
// in a session could ask. A CI check, a script, or a person at a terminal could
// not — and "what does this endpoint touch" is exactly the question someone asks
// while writing the change, not while chatting about it.
func (r *Runner) Endpoint(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("endpoint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		asJSON    = fs.Bool("json", false, "emit the result as JSON")
		maxRoutes = fs.Int("max-routes", 25, "how many matched endpoints to follow")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" endpoint [flags] <endpoint> [repo_path|config_path]\n\n"+
				"Report what changing an HTTP endpoint reaches: the controller serving it, the\n"+
				"models that controller touches, the models associated with those, the tables\n"+
				"behind them, and the callers — including the frontend screen a calling route\n"+
				"module implements.\n\n"+
				"The endpoint is matched as a substring of the path, optionally prefixed with a\n"+
				"verb: 'GET /v1/candidates' or just '/v1/candidates'.\n\n"+
				"Client call sites and mock-server routes are excluded: this answers about what\n"+
				"the application serves.\n\n"+
				"Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	query := rest[0]
	target := ""
	if len(rest) > 1 {
		target = rest[1]
	}

	tgt := r.resolveTarget(target)
	fmt.Fprintf(os.Stderr, r.name()+" endpoint: %s\n", tgt.configNote)

	// Read-only, like coverage and check: reporting on a graph must not rewrite it.
	tgt.engine.SetPersistCache(false)
	for i, repoPath := range tgt.repoPaths {
		if _, err := tgt.engine.GenerateSnapshot(ctx, repoPath, i > 0); err != nil {
			fmt.Fprintf(os.Stderr, "snapshot generation failed for %s: %v\n", repoPath, err)
			os.Exit(2)
		}
	}

	// Callers come from the cross-repo linker's own matching, under this config's linking
	// vocabulary, so a call it linked is a caller here too.
	linkVocab, err := tgt.engine.Config().LinkingVocab()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid linking vocabulary: %v\n", err)
		os.Exit(2)
	}
	store := tgt.engine.Store()
	result := endpoint.Analyze(store, query, *maxRoutes, httpsignal.NewCallerFinder(routeindex.New(linkVocab), store.All()))
	if *asJSON {
		out, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to encode result: %v\n", err)
			os.Exit(2)
		}
		fmt.Println(string(out))
		return
	}

	fmt.Println(result.Summary)
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Printf("\n%s:\n", title)
		for _, item := range items {
			fmt.Printf("  %s\n", item)
		}
	}
	routes := make([]string, 0, len(result.Routes))
	for _, route := range result.Routes {
		line := strings.TrimSpace(route.Method + " " + route.Path)
		if route.Handler != "" {
			line += "  -> " + route.Handler
		}
		routes = append(routes, line)
	}
	section("routes", routes)
	section("controllers", result.Controllers)
	section("models", result.Models)
	section("associated models", result.Associated)
	section("tables", result.Tables)

	callers := make([]string, 0, len(result.Callers))
	for _, caller := range result.Callers {
		line := caller.File
		if caller.Screen != "" {
			line += "  (screen " + caller.Screen + ")"
		}
		callers = append(callers, line)
	}
	section("callers", callers)
}
