package command

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/enola-labs/enola/internal/providers"
	"github.com/enola-labs/enola/internal/providers/rubydex"
	"github.com/enola-labs/enola/pkg/check"
)

// Providers manages the fact providers the binary carries itself. Today that
// is one: `providers fetch rubydex` puts the pinned Rubydex engine library in
// enola's cache so a `providers:` entry named rubydex runs; `providers list`
// reports each built-in and whether it is ready. Fetching is the only time
// a provider touches the network, never at snapshot time.
func (r *Runner) Providers(ctx context.Context, args []string) {
	if HelpRequested(args) {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" providers <list|fetch> [name]\n\n"+
			"The fact providers this binary carries itself.\n\n"+
			"  list         name each built-in provider and whether it is ready\n"+
			"  fetch <name> download the pinned engine library of a built-in provider,\n"+
			"               verify its published digest, and cache it. After\n"+
			"               `fetch rubydex`, a `providers:` entry named rubydex with no\n"+
			"               command runs in-process. The only network access a provider\n"+
			"               ever makes, and never at snapshot time.\n\n"+
			"Built-ins: "+strings.Join(providers.BuiltInNames(), ", ")+"\n\n"+
			"Exit codes:\n"+
			"  0  listed, fetched, or already installed\n"+
			"  1  the download or its verification failed\n"+
			"  2  the command could not run\n"+
			r.HelpFooter("providers"))
		os.Exit(check.StatusUsageError.ExitCode())
	}
	if len(args) == 0 {
		r.cmdFatal("providers", "usage: %s providers list | fetch <name>", r.name())
	}
	switch args[0] {
	case "list":
		for _, name := range providers.BuiltInNames() {
			fmt.Printf("  %s  %s\n", name, builtInStatus(name))
		}
		return
	case "fetch":
		if len(args) != 2 {
			r.cmdFatal("providers", "usage: %s providers fetch <name> (built-ins: %s)", r.name(), strings.Join(providers.BuiltInNames(), ", "))
		}
		if args[1] != "rubydex" && args[1] != "prism" {
			r.cmdFatal("providers", "%q is not a provider this enola carries (built-ins: %s)", args[1], strings.Join(providers.BuiltInNames(), ", "))
		}
		if path, installed := rubydex.Installed(); installed {
			fmt.Printf("rubydex %s is already installed at %s\n", rubydex.Version, path)
			return
		}
		path, err := rubydex.Fetch(ctx, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s providers fetch rubydex: %v\n", r.name(), err)
			os.Exit(1)
		}
		fmt.Printf("rubydex %s installed at %s\n", rubydex.Version, path)
		return
	default:
		r.cmdFatal("providers", "unknown providers command %q (list, fetch)", args[0])
	}
}

func builtInStatus(name string) string {
	if name != "rubydex" && name != "prism" {
		return "ready"
	}
	path, installed := rubydex.Installed()
	if installed {
		return fmt.Sprintf("%s at %s", rubydex.Version, path)
	}
	return fmt.Sprintf("not installed; run `%s`", rubydex.FetchHint)
}
