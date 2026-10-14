package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// goCommands extracts a module written from files and returns function -> command.
func goCommands(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	repo := t.TempDir()
	files["go.mod"] = "module example.com/tool\n\ngo 1.22\n"
	rels := make([]string, 0, len(files))
	for rel, content := range files {
		abs := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	ff, err := New().Extract(context.Background(), repo, rels)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol {
			continue
		}
		if _, leaked := f.Props[dispatchProp]; leaked {
			t.Errorf("%s carries %s", f.Name, dispatchProp)
		}
		if c := f.PropString(commandProp); c != "" {
			out[f.Name] = c
		}
	}
	return out
}

// A string-keyed dispatch names the function each case hands its command to, and a
// dispatch inside a command's function is that command's subcommands. A case that
// does its work inline names no function, and a flag switch is no dispatch.
func TestCommands_StringKeyedDispatch(t *testing.T) {
	got := goCommands(t, map[string]string{
		"cmd/tool/main.go": `package main

import (
	"os"

	"example.com/tool/command"
)

func main() {
	r := &command.Runner{}
	r.Dispatch(os.Args[1:])
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--version":
			version()
		case "--help":
			help()
		}
	}
}

func version() {}
func help()    {}
`,
		"command/command.go": `package command

import "fmt"

type Runner struct{}

func (r *Runner) Dispatch(args []string) {
	switch args[0] {
	case "check":
		r.Check(args[1:])
	case "providers":
		r.Providers(args[1:])
	case "version":
		fmt.Println("v1")
	}
}

func (r *Runner) Check(args []string) { r.verdict() }
func (r *Runner) verdict()            {}

func (r *Runner) Providers(args []string) {
	switch args[0] {
	case "list":
		for _, n := range names() {
			fmt.Println(n)
		}
	case "fetch":
		r.fetch(args[1:])
	}
}

func (r *Runner) fetch(args []string) {}
func names() []string             { return nil }
`,
	})
	want := map[string]string{
		"command.Runner.Check":     "tool check",
		"command.Runner.Providers": "tool providers",
		"command.Runner.fetch":     "tool providers fetch",
	}
	for fn, cmd := range want {
		if got[fn] != cmd {
			t.Errorf("%s command = %q, want %q (have %v)", fn, got[fn], cmd, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("commands = %v, want exactly %v", got, want)
	}
}

// A cobra or urfave/cli command literal names the function it runs.
func TestCommands_CobraAndUrfaveLiterals(t *testing.T) {
	got := goCommands(t, map[string]string{
		"cmd/serve.go": `package cmd

import "github.com/spf13/cobra"

var serveCmd = &cobra.Command{
	Use:   "serve [flags]",
	Short: "Run the server",
	RunE:  runServe,
}

var inlineCmd = &cobra.Command{
	Use: "inline",
	Run: func(cmd *cobra.Command, args []string) {},
}

func runServe(cmd *cobra.Command, args []string) error { return nil }
`,
		"cmd/web.go": `package cmd

import "github.com/urfave/cli/v2"

var CmdWeb = &cli.Command{
	Name:   "web",
	Action: runWeb,
}

func runWeb(c *cli.Context) error { return nil }

var pluginCommands = []*cli.Command{
	{Name: "install", Action: wrap(installCommand)},
	{Name: "server", Action: func(c *cli.Context) error { return RunServer(c) }},
}

func wrap(f func(*cli.Context) error) func(*cli.Context) error { return f }
func installCommand(c *cli.Context) error                       { return nil }
func RunServer(c *cli.Context) error                            { return nil }
`,
	})
	want := map[string]string{
		"cmd.runServe":       "tool serve",
		"cmd.runWeb":         "tool web",
		"cmd.installCommand": "tool install",
		"cmd.RunServer":      "tool server",
	}
	for fn, cmd := range want {
		if got[fn] != cmd {
			t.Errorf("%s command = %q, want %q (have %v)", fn, got[fn], cmd, got)
		}
	}
}
