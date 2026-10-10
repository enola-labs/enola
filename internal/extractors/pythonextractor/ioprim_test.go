package pythonextractor

import (
	"slices"
	"testing"
)

func TestPyLibraryCall(t *testing.T) {
	tests := []struct {
		target string
		want   int
	}{
		// The same member, reached through either module path.
		{"sqlalchemy.orm.Session.execute", pyCallIO},
		{"sqlalchemy.orm.session.Session.execute", pyCallIO},
		{"sqlalchemy.engine.base.Connection.execute", pyCallIO},
		{"sqlalchemy.Inspector.get_columns", pyCallIO},
		// A method is not the library's function of the same name, nor the reverse.
		{"sqlalchemy.orm.Session.delete", pyCallPure},
		{"sqlalchemy.delete", pyCallPure},
		{"sqlalchemy.update", pyCallPure},
		{"sqlalchemy.sql.expression.Select.where", pyCallPure},
		// The head of a query chain whose end is not typed.
		{"sqlalchemy.orm.Session.query", pyCallUnknown},
		{"requests.get", pyCallIO},
		{"requests.sessions.Session.send", pyCallIO},
		{"requests.cookies.RequestsCookieJar.set", pyCallPure},
		{"urllib.request.urlopen", pyCallIO},
		{"urllib.parse.urlparse", pyCallPure},
		{"os.path.exists", pyCallIO},
		{"os.path.join", pyCallPure},
		{"os.remove", pyCallIO},
		{"pathlib.Path.exists", pyCallIO},
		{"pathlib.Path.joinpath", pyCallPure},
		{"subprocess.run", pyCallIO},
		{"re.search", pyCallPure},
		{"structlog.stdlib.BoundLogger.error", pyCallPure},
		// A type of a module that is otherwise not described.
		{"io.StringIO.write", pyCallPure},
		{"io.BufferedIOBase.read", pyCallUnknown},
		// These run what they are handed.
		{"asyncio.gather", pyCallUnknown},
		{"asyncio.Event.wait", pyCallPure},
		{"asyncio.open_connection", pyCallIO},
		// An open list is silent on what it does not name.
		{"alembic.op.execute", pyCallIO},
		{"alembic.op.get_bind", pyCallUnknown},
		// Not described at all, and a name that only starts like one that is.
		{"yaml.safe_load", pyCallUnknown},
		{"requests_toolbelt.MultipartEncoder.read", pyCallUnknown},
		{"requests", pyCallUnknown},
	}
	for _, tt := range tests {
		if got := pyLibraryCall(tt.target); got != tt.want {
			t.Errorf("pyLibraryCall(%q) = %d, want %d", tt.target, got, tt.want)
		}
	}
}

func pyStrings(v any) []string {
	s, _ := v.([]string)
	return s
}

// A call resolved to a library member is I/O, or is not, by what the member is,
// and the function's fact says which calls were which.
func TestLibraryCallsAreClassedByMember(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"app/__init__.py": "",
		"app/store.py": `import re

from sqlalchemy import update
from sqlalchemy.orm import Session


def purge(session: Session, rows):
    for row in rows:
        session.delete(row)
    session.flush()


def rename(session: Session, rows):
    for row in rows:
        stmt = update(Row).where(Row.id == row.id)
        session.execute(stmt)


def matching(rows, pattern):
    return [row for row in rows if re.search(pattern, row.name)]


def caller(session, rows):
    rename(session, rows)
`,
	})

	rename := pySym(t, ff, "store.rename")
	if got := pyStrings(rename.PropAny("io_calls")); !slices.Contains(got, "sqlalchemy.orm.Session.execute") {
		t.Errorf("rename io_calls = %v, want Session.execute", got)
	}
	if got := pyStrings(rename.PropAny("pure_calls")); !slices.Contains(got, "sqlalchemy.update") {
		t.Errorf("rename pure_calls = %v, want sqlalchemy.update", got)
	}
	if !pyIO(pySym(t, ff, "store.caller")) {
		t.Error("caller reaches Session.execute through rename and must be performs_io")
	}

	purge := pySym(t, ff, "store.purge")
	if got := pyStrings(purge.PropAny("pure_calls")); !slices.Contains(got, "sqlalchemy.orm.Session.delete") {
		t.Errorf("purge pure_calls = %v, want Session.delete: it marks the object, the flush is the round trip", got)
	}
	if got := pyStrings(purge.PropAny("io_calls")); !slices.Contains(got, "sqlalchemy.orm.Session.flush") {
		t.Errorf("purge io_calls = %v, want Session.flush", got)
	}

	matching := pySym(t, ff, "store.matching")
	if got := pyStrings(matching.PropAny("pure_calls")); !slices.Contains(got, "re.search") {
		t.Errorf("matching pure_calls = %v, want re.search", got)
	}
	if pyIO(matching) {
		t.Error("matching only runs a regex and must not be performs_io")
	}
}

// A module of the repository that shares a library's name is the repository's.
func TestLibraryTableDoesNotClaimARepositoryModule(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"requests/__init__.py": "def get(url):\n    return url\n",
		"app/__init__.py":      "",
		"app/client.py": `import requests


def fetch_all(urls):
    return [requests.get(u) for u in urls]
`,
	})
	fetch := pySym(t, ff, "client.fetch_all")
	if got := pyStrings(fetch.PropAny("io_calls")); len(got) != 0 {
		t.Errorf("io_calls = %v, want none: requests.get is this repository's function", got)
	}
}
