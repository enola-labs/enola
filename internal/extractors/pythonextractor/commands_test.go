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

// A command is named by the path of groups it is reached through: a group by its
// declared name, nested groups in order, a command attached with add_command under
// the group it is attached to, and an add_command name= over the command's own.
func TestCommands_ComposedThroughGroups(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"breeze/__init__.py":          ``,
		"breeze/commands/__init__.py": ``,
		"breeze/main.py": `import click
from breeze.commands.testing_commands import testing_group
from breeze.commands.auth import login, auth_group

@click.group()
def main():
    pass

main.add_command(testing_group)
main.add_command(login)
main.add_command(auth_group, name="authn")
`,
		"breeze/commands/testing_commands.py": `import click

@click.group(cls=BreezeGroup, name="testing", help="Run tests")
def testing_group():
    pass

@testing_group.command(name="tests")
def run_tests():
    pass

@testing_group.group()
def docker_compose():
    pass

@docker_compose.command()
def up_all():
    pass
`,
		"breeze/commands/kustomize.py": `import click
from breeze.commands.testing_commands import testing_group

@testing_group.command()
def smoke_test():
    pass
`,
		"breeze/commands/auth.py": `import click

@click.command()
def login():
    pass

@click.group(name="auth")
def auth_group():
    pass

@auth_group.command()
def whoami():
    pass

@click.command()
def never_attached():
    pass
`,
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.PropString("command") != "" {
			got[f.Name] = f.PropString("command")
		}
		for _, p := range []string{propCommandLeaf, propCommandParent, propCommandGroup, propCommandAttaches} {
			if f.PropAny(p) != nil {
				t.Errorf("%s keeps the working prop %s", f.Name, p)
			}
		}
	}
	want := map[string]string{
		"breeze/commands/testing_commands.run_tests": "testing tests",
		"breeze/commands/testing_commands.up_all":    "testing docker-compose up-all",
		"breeze/commands/kustomize.smoke_test":       "testing smoke-test",
		"breeze/commands/auth.login":                 "login",
		"breeze/commands/auth.whoami":                "authn whoami",
		"breeze/commands/auth.never_attached":        "never-attached",
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

// An MCP tool declared with a decorator is named by its given name or its function's.
func TestTools_DecoratedFunctions(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"server/server.py": `import fastmcp

mcp = fastmcp.FastMCP("memory")

@mcp.tool()
async def remember(data: str):
    pass

@registry.tool(name="recall_memory", tags={"memory"})
async def recall(query: str):
    pass
`,
		"superset/chart/delete_chart.py": `from superset_core.mcp.decorators import tool

@tool(annotations=None)
async def delete_chart(chart_id: int):
    pass
`,
		"other/plain.py": `from mylib import tool

@tool()
def not_a_tool():
    pass
`,
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.PropString("tool") != "" {
			got[f.Name] = f.PropString("tool")
			if !f.PropBool("framework_registered") {
				t.Errorf("%s is not marked framework_registered", f.Name)
			}
		}
	}
	want := map[string]string{
		"server/server.remember":                   "remember",
		"server/server.recall":                     "recall_memory",
		"superset/chart/delete_chart.delete_chart": "delete_chart",
	}
	for fn, tool := range want {
		if got[fn] != tool {
			t.Errorf("%s tool = %q, want %q (have %v)", fn, got[fn], tool, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tools = %v, want exactly %v", got, want)
	}
}
