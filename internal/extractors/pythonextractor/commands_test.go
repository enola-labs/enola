package pythonextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A click or typer command is named as a user types it: its given name, or the
// function's with dashes, after its group when the group has a name of its own.
func TestCommands_ClickAndTyper(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"superset/cli/main.py": `import click

@click.group()
def superset():
    pass

@superset.command()
def load_examples():
    pass

@superset.command(name="set-db")
def set_database_uri():
    pass
`,
		"tool/app.py": `import typer

app = typer.Typer()

@app.command("init")
def initialize():
    pass

@app.command()
def run_job():
    pass
`,
		"other/flask_script.py": `class Manager:
    pass

manager = Manager()

@manager.command()
def not_a_click_command():
    pass
`,
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.PropString("command") != "" {
			got[f.Name] = f.PropString("command")
		}
	}
	want := map[string]string{
		"superset/cli/main.load_examples":    "superset load-examples",
		"superset/cli/main.set_database_uri": "superset set-db",
		"tool/app.initialize":                "init",
		"tool/app.run_job":                   "run-job",
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
