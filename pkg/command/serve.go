package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/version"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// minTokenLen is the shortest bearer token serve accepts: 32 characters, the length
// of 16 random bytes in hex, so a token cannot be a word someone typed.
const minTokenLen = 32

// rootsFlag collects repeated --root flags.
type rootsFlag []string

func (f *rootsFlag) String() string     { return strings.Join(*f, ",") }
func (f *rootsFlag) Set(v string) error { *f = append(*f, v); return nil }

// Serve is `enola serve`: the MCP tools over Streamable HTTP, for a service that calls
// enola as a tool rather than an agent that started it. It is a subcommand of its own,
// never a flag or config key of the stdio server, so that nothing committed to a
// repository can open a port.
func (r *Runner) Serve(ctx context.Context, args []string) {
	os.Exit(r.runServe(ctx, args, os.Stderr))
}

func (r *Runner) runServe(ctx context.Context, args []string, stderr io.Writer) int {
	say := func(format string, a ...any) {
		_, _ = fmt.Fprintf(stderr, r.name()+" serve: "+format+"\n", a...)
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var roots rootsFlag
	fs.Var(&roots, "root", "a directory consumers may name paths in; repeat for several (required)")
	var (
		addr           = fs.String("http", "127.0.0.1:7341", "address to listen on; anything but loopback should sit behind TLS")
		tokenFile      = fs.String("token-file", "", "file of accepted bearer tokens, one per line, readable by its owner only (required)")
		maxConcurrent  = fs.Int("max-concurrent", runtime.NumCPU(), "tool calls running at once; more wait")
		callTimeout    = fs.Duration("call-timeout", 15*time.Minute, "cancel a tool call running longer (0 for no limit)")
		sessionTimeout = fs.Duration("session-timeout", 30*time.Minute, "close a session idle this long")
		outputRoot     = fs.String("output-root", "", "write every repository's artifacts under this directory instead of inside the repository")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr,
			"Usage: "+r.name()+" serve --root DIR --token-file FILE [flags] [repo_path|config_path]\n\n"+
				"Serve the MCP tools over Streamable HTTP at /mcp, for a service that calls enola\n"+
				"as a tool. Every request needs a bearer token from --token-file. Every path a\n"+
				"call names, and every repository it reaches, must lie inside a --root; others\n"+
				"are refused. A repository or config given here has its graph restored from disk\n"+
				"at start; otherwise consumers index with generate_snapshot. Nothing is written\n"+
				"to ~/.enola/receipt.json, and nothing into the repositories when --output-root\n"+
				"is set. One line per tool call is logged to stderr.\n\n"+
				"Exit codes:\n"+
				"  0  stopped by a signal\n"+
				"  1  the server failed\n"+
				"  2  the command could not run\n\nFlags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "serve")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(roots) == 0 || *tokenFile == "" || fs.NArg() > 1 || *maxConcurrent < 1 || *callTimeout < 0 || *sessionTimeout <= 0 {
		fs.Usage()
		return 2
	}

	tokens, err := readTokenFile(*tokenFile)
	if err != nil {
		say("%v", err)
		return 2
	}
	cfgPath, repoArg := "mcp-arch.yaml", ""
	restore := false
	if fs.NArg() == 1 {
		switch arg := fs.Arg(0); {
		case IsDirectory(arg):
			repoArg = arg
		case FileExists(arg):
			cfgPath = arg
		default:
			say("%s is neither a directory nor a config file", arg)
			return 2
		}
		restore = true
	}
	eng, cfg, err := r.newEngine(bootstrap.Options{ConfigPath: cfgPath})
	if err != nil {
		say("%v", err)
		return 2
	}
	if *outputRoot != "" {
		abs, err := filepath.Abs(*outputRoot)
		if err == nil {
			err = os.MkdirAll(abs, 0o755)
		}
		if err == nil {
			err = checkWritable(abs)
		}
		if err != nil {
			say("--output-root: %v", err)
			return 2
		}
		cfg.Output.Root = abs
	}
	var restorePaths []string
	if restore {
		if repoArg != "" {
			abs, err := filepath.Abs(repoArg)
			if err != nil {
				say("%v", err)
				return 2
			}
			cfg.Repo, cfg.Repos = abs, nil
		}
		if restorePaths, err = cfg.RepoPaths(); err != nil {
			say("%v", err)
			return 2
		}
	} else {
		// No default repository: a call that names none must be refused, not handed
		// the directory the service manager happened to start this process in.
		cfg.Repo, cfg.Repos = "", nil
	}

	srv, resolved, err := bootstrap.NewServedServer(eng, cfg, bootstrap.ServeOptions{
		Roots:         roots,
		MaxConcurrent: *maxConcurrent,
		CallTimeout:   *callTimeout,
		Restore:       restorePaths,
	})
	if err != nil {
		say("%v", err)
		return 2
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		say("%v", err)
		return 1
	}
	loopback := isLoopback(ln.Addr())
	_, _ = fmt.Fprintf(stderr, "%s serve %s: MCP over Streamable HTTP at http://%s/mcp\n", r.name(), version.Version, ln.Addr())
	_, _ = fmt.Fprintf(stderr, "  Roots:       %s\n", strings.Join(resolved, ", "))
	if cfg.Output.Root != "" {
		_, _ = fmt.Fprintf(stderr, "  Output:      %s\n", cfg.Output.Root)
	}
	_, _ = fmt.Fprintf(stderr, "  Tokens:      %d accepted\n", len(tokens))
	_, _ = fmt.Fprintf(stderr, "  Limits:      %d concurrent calls, %s per call, sessions idle %s\n", *maxConcurrent, *callTimeout, *sessionTimeout)
	if restore {
		_, _ = fmt.Fprintf(stderr, "  Restoring:   %s\n", strings.Join(restorePaths, ", "))
	}
	if !loopback {
		_, _ = fmt.Fprintf(stderr, "  Warning:     listening beyond loopback; terminate TLS in front of it\n")
	}

	err = srv.RunHTTP(ctx, ln, bootstrap.HTTPOptions{Tokens: tokens, SessionTimeout: *sessionTimeout})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("serve: %v", err)
		return 1
	}
	return 0
}

// readTokenFile reads the accepted bearer tokens: one per line, blank lines and lines
// starting with # skipped. It refuses a file other users can read, the same way ssh
// refuses a private key, and a token shorter than minTokenLen.
func readTokenFile(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("token file: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("token file %s is accessible by other users (mode %s); chmod 600 it", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("token file: %w", err)
	}
	var tokens []string
	for i, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if len(t) < minTokenLen {
			return nil, fmt.Errorf("token file %s line %d: a token needs at least %d characters", path, i+1, minTokenLen)
		}
		if strings.ContainsAny(t, " \t") {
			return nil, fmt.Errorf("token file %s line %d: a token cannot contain whitespace", path, i+1)
		}
		tokens = append(tokens, t)
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("token file %s holds no tokens", path)
	}
	return tokens, nil
}

// checkWritable fails now, at start, for a directory serve could not write a snapshot
// into, rather than on the first generate_snapshot a consumer makes.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".enola-write-check-")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

func isLoopback(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}
