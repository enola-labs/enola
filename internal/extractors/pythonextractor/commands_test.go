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

// An argparse command is named by the subparsers it is added through, and a command
// table (a call with name= and func=, grouped by one with name= and subcommands=)
// by the groups whose subcommands list holds it. func= names the handler directly
// or as a dotted import path handed to a loader.
func TestCommands_ArgparseAndCommandTables(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"app/__init__.py":              ``,
		"app/cli/__init__.py":          ``,
		"app/cli/commands/__init__.py": ``,
		"app/cli/cli_config.py": `import argparse
from typing import NamedTuple

def lazy_load_command(import_path):
    pass

class ActionCommand(NamedTuple):
    name: str

class GroupCommand(NamedTuple):
    name: str

DAGS_COMMANDS = (
    ActionCommand(name="list", help="List DAGs", func=lazy_load_command("app.cli.commands.dag_command.dag_list"), args=()),
    ActionCommand(name="trigger", help="Trigger", func=lazy_load_command("app.cli.commands.dag_command.dag_trigger"), args=()),
)

core_commands: list = [
    GroupCommand(name="dags", help="Manage DAGs", subcommands=DAGS_COMMANDS),
    ActionCommand(name="scheduler", help="Run", func=lazy_load_command("app.cli.commands.scheduler_command.scheduler"), args=()),
]

ROLES_COMMANDS = (
    ActionCommand(name="list", help="List roles", func=lazy_load_command("app.cli.commands.role_command.roles_list"), args=()),
)

USERS_COMMANDS = (
    ActionCommand(name="add", help="Add a user", func=lazy_load_command("app.cli.commands.role_command.users_add"), args=()),
)

def get_auth_commands():
    commands = [
        GroupCommand(name="roles", help="Manage roles", subcommands=ROLES_COMMANDS),
    ]
    commands.append(GroupCommand(name="users", help="Manage users", subcommands=USERS_COMMANDS))
    return commands

EDGE_COMMANDS: list = [
    ActionCommand(name="stop", help="Stop", func=lazy_load_command("app.cli.commands.role_command.edge_stop"), args=()),
]

def get_edge_commands():
    return [GroupCommand(name="edge", help="Edge", subcommands=EDGE_COMMANDS)]
`,
		"app/cli/commands/role_command.py": `def roles_list(args):
    pass

def users_add(args):
    pass

def edge_stop(args):
    pass
`,
		"app/cli/commands/dag_command.py": `def dag_list(args):
    pass

def dag_trigger(args):
    pass
`,
		"app/cli/commands/scheduler_command.py": `def scheduler(args):
    pass
`,
		"tool/__init__.py": ``,
		"tool/handlers.py": `def run_build(args):
    pass
`,
		"tool/main.py": `import argparse
from tool.handlers import run_build

def handle_clean(args):
    pass

def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers()
    build = sub.add_parser("build")
    build.set_defaults(func=run_build)
    cache = sub.add_parser("cache")
    cache_sub = cache.add_subparsers()
    cache_sub.add_parser("clean").set_defaults(func=handle_clean)
`,
		"agent/tools.py": `from langchain.tools import Tool

def search(q):
    pass

TOOLS = [Tool(name="search", func=search, description="Search")]
`,
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.PropString("command") != "" {
			got[f.Name] = f.PropString("command")
		}
		if f.PropAny(propCommandTable) != nil {
			t.Errorf("%s keeps the working prop %s", f.Name, propCommandTable)
		}
	}
	want := map[string]string{
		"app/cli/commands/dag_command.dag_list":        "dags list",
		"app/cli/commands/dag_command.dag_trigger":     "dags trigger",
		"app/cli/commands/scheduler_command.scheduler": "scheduler",
		"app/cli/commands/role_command.roles_list":     "roles list",
		"app/cli/commands/role_command.users_add":      "users add",
		"app/cli/commands/role_command.edge_stop":      "edge stop",
		"tool/handlers.run_build":                      "build",
		"tool/main.handle_clean":                       "cache clean",
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

// A root click group is named by the console script that runs it, whether the
// script names the group, a function that calls it, or a name its module only
// imports from where the group is defined; a group no script runs keeps its own
// name, or none when that name is generic.
func TestCommands_RootGroupNamedByConsoleScript(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"pyproject.toml": `[project]
name = "dispatch"

[project.scripts]
dispatch = "dispatch.cli:entrypoint"
breeze = "breeze.main:main"
ctl = "ctl.entry:main"
`,
		"ctl/__init__.py":          ``,
		"ctl/commands/__init__.py": ``,
		"ctl/entry.py": `import click
from ctl.commands.main_command import main
from ctl.commands.release_command import release_group

main.add_command(release_group)
`,
		"ctl/commands/release_command.py": `from ctl.commands.release_group import release_group
`,
		"ctl/commands/release_group.py": `import click

@click.group(name="release")
def release_group():
    pass

@release_group.command()
def publish():
    pass
`,
		"ctl/commands/main_command.py": `import click

@click.group()
def main():
    pass

@main.command()
def status():
    pass
`,
		"dispatch/__init__.py": ``,
		"dispatch/cli.py": `import click

@click.group()
def dispatch_cli():
    pass

@dispatch_cli.group("database")
def dispatch_database():
    pass

@dispatch_database.command("upgrade")
def upgrade_database():
    pass

def entrypoint():
    try:
        dispatch_cli()
    except Exception:
        pass
`,
		"breeze/__init__.py": ``,
		"breeze/main.py": `import click

@click.group()
def main():
    pass

@main.command()
def shell():
    pass
`,
		"tools/admin.py": `import click

@click.group()
def admin():
    pass

@admin.command()
def reset():
    pass
`,
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.PropString("command") != "" {
			got[f.Name] = f.PropString("command")
		}
		if f.PropAny(propConsoleScripts) != nil {
			t.Errorf("%s keeps the working prop %s", f.Name, propConsoleScripts)
		}
	}
	want := map[string]string{
		"dispatch/cli.upgrade_database":      "dispatch database upgrade",
		"breeze/main.shell":                  "breeze shell",
		"ctl/commands/main_command.status":   "ctl status",
		"ctl/commands/release_group.publish": "ctl release publish",
		"tools/admin.reset":                  "admin reset",
	}
	for fn, cmd := range want {
		if got[fn] != cmd {
			t.Errorf("%s command = %q, want %q (have %v)", fn, got[fn], cmd, got)
		}
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
