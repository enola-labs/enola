package pythonextractor

import (
	"slices"
	"testing"
)

func TestPyValueType(t *testing.T) {
	tests := []struct{ ann, want string }{
		{"S3Client", "S3Client"},
		{"storage.Client", "storage.Client"},
		{`"S3Client"`, "S3Client"},
		{"S3Client | None", "S3Client"},
		{"None | S3Client", "S3Client"},
		{"Optional[S3Client]", "S3Client"},
		{"typing.Optional[storage.Client]", "storage.Client"},
		{"Queue[Job]", "Queue"},
		// A container, a promise and a choice name no single type of the value.
		{"list[Job]", ""},
		{"dict[str, Any]", ""},
		{"Iterator[Job]", ""},
		{"Awaitable[Job]", ""},
		{"S3Client | GCSClient", ""},
		{"Any", ""},
		{"None", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := pyValueType(tt.ann); got != tt.want {
			t.Errorf("pyValueType(%q) = %q, want %q", tt.ann, got, tt.want)
		}
	}
}

const accessorHook = `from google.cloud import storage
from sqlalchemy.orm import Session

from app.store import Store


def make_store() -> Store:
    return Store()


class Hook:
    def get_conn(self) -> storage.Client:
        return storage.Client()

    def get_store(self) -> Store | None:
        return Store()

    def get_untyped(self):
        return Store()

    def session(self) -> Session:
        return Session()

    def chained(self, name):
        self.get_conn().delete_blob(name)
        self.get_store().save(name)
        self.get_untyped().save(name)

    def through_local(self, names):
        store = self.get_store()
        for name in names:
            store.save(name)

    def through_with(self, rows):
        with self.session() as s:
            for row in rows:
                s.execute(row)

    def through_function(self, name):
        make_store().save(name)

    def rebound(self, name, other):
        store = self.get_store()
        store = other
        store.load(name)

    def unpacked(self, name, pair):
        store = self.get_store()
        store, _ = pair
        store.load(name)
`

const accessorStore = `class Store:
    def save(self, name):
        open(name, "w").close()

    def load(self, name):
        return name
`

// A call on an accessor's result resolves by the accessor's return annotation.
func TestCallOnAccessorResultResolvesByReturnAnnotation(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"app/__init__.py": "",
		"app/store.py":    accessorStore,
		"app/hook.py":     accessorHook,
	})

	chained := pySym(t, ff, "Hook.chained")
	if !pyCallsTo(chained, "store.Store.save") {
		t.Errorf("self.get_store().save must resolve to Store.save; relations: %v", chained.Relations)
	}
	n := 0
	for _, r := range chained.Relations {
		if r.Target == "app/store.Store.save" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("an unannotated accessor must add no edge: %d edges to Store.save, want 1", n)
	}
	if !pyIO(chained) {
		t.Error("chained reaches open() through Store.save and must be performs_io")
	}

	for _, name := range []string{"Hook.through_local", "Hook.through_function"} {
		f := pySym(t, ff, name)
		if !pyCallsTo(f, "store.Store.save") {
			t.Errorf("%s must call Store.save; relations: %v", name, f.Relations)
		}
	}
	local := pySym(t, ff, "Hook.through_local")
	if got := pyStrings(local.PropAny("calls_in_loop")); !slices.Contains(got, "app/store.Store.save") {
		t.Errorf("through_local calls_in_loop = %v, want Store.save", got)
	}

	// The context manager's value is the accessor's, and it is a library type.
	with := pySym(t, ff, "Hook.through_with")
	if got := pyStrings(with.PropAny("io_calls")); !slices.Contains(got, "sqlalchemy.orm.Session.execute") {
		t.Errorf("through_with io_calls = %v, want Session.execute", got)
	}

	// A name bound twice has no one type.
	for _, name := range []string{"Hook.rebound", "Hook.unpacked"} {
		if f := pySym(t, ff, name); pyCallsTo(f, "store.Store.load") {
			t.Errorf("%s rebinds the local and must not resolve the call; relations: %v", name, f.Relations)
		}
	}
}

// A call on an imported module-level name resolves by the type the name is
// declared to hold, whether it is imported from its module or from a package that
// re-exports it.
func TestCallOnModuleGlobalResolvesByDeclaredType(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"app/__init__.py": "from app.extensions import security_manager\n",
		"app/security.py": `class SecurityManager:
    def can_access(self, chart):
        return open(chart).read()
`,
		"app/flags.py": `class FlagManager:
    def enabled(self, name):
        return name
`,
		"app/extensions/__init__.py": `from sqlalchemy.orm import Session

from app.flags import FlagManager
from app.security import SecurityManager

security_manager: SecurityManager = make_proxy()
flags = FlagManager()
session: Session = make_proxy()
twice = FlagManager()
twice = SecurityManager()
`,
		"app/api/__init__.py": "",
		"app/api/views.py": `from app import security_manager
from app.extensions import flags, session, twice
from app.extensions import security_manager as sm


def visible(charts):
    return [c for c in charts if security_manager.can_access(c)]


def also_visible(charts):
    return [c for c in charts if sm.can_access(c)]


def flagged(names):
    return [n for n in names if flags.enabled(n)]


def run(statements):
    for s in statements:
        session.execute(s)


def ambiguous(name):
    return twice.enabled(name)
`,
	})

	for _, name := range []string{"views.visible", "views.also_visible"} {
		f := pySym(t, ff, name)
		if !pyCallsTo(f, "app/security.SecurityManager.can_access") {
			t.Errorf("%s must call SecurityManager.can_access; relations: %v", name, f.Relations)
		}
		if got := pyStrings(f.PropAny("calls_in_loop")); !slices.Contains(got, "app/security.SecurityManager.can_access") {
			t.Errorf("%s calls_in_loop = %v, want the resolved method", name, got)
		}
		if !pyIO(f) {
			t.Errorf("%s reaches open() through the security manager and must be performs_io", name)
		}
	}
	if f := pySym(t, ff, "views.flagged"); !pyCallsTo(f, "app/flags.FlagManager.enabled") {
		t.Errorf("a global assigned from a constructor has that type; relations: %v", f.Relations)
	}
	// A global declared to hold a library type: the call is that type's method.
	if got := pyStrings(pySym(t, ff, "views.run").PropAny("io_calls")); !slices.Contains(got, "sqlalchemy.orm.Session.execute") {
		t.Errorf("run io_calls = %v, want Session.execute through the annotated global", got)
	}
	if f := pySym(t, ff, "views.ambiguous"); pyCallsTo(f, "FlagManager.enabled") || pyCallsTo(f, "SecurityManager.enabled") {
		t.Errorf("a global bound to two types has none; relations: %v", f.Relations)
	}
}

// A statement run per row on a connection nothing types is a call in a loop, and
// is named as the source writes it.
func TestUntypedIOCallInLoopIsRecordedAsWritten(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"conn/__init__.py": "def execute(stmt):\n    return stmt\n",
		"app/__init__.py":  "",
		"app/migrate.py": `import sys


def backfill(rows):
    conn = get_bind()
    for row in rows:
        conn.execute(make_update(row))
        conn.describe(row)
        conn.fetchall()
        sys.stdout.flush()


def once(rows):
    conn = get_bind()
    conn.execute(make_update(rows))


class Loader:
    def drain(self, batches):
        for batch in batches:
            self.cursor.executemany(batch)
            self.pool().execute(batch)
`,
	})

	backfill := pySym(t, ff, "migrate.backfill")
	if got := pyStrings(backfill.PropAny("calls_in_loop")); !slices.Contains(got, "conn.execute") {
		t.Errorf("backfill calls_in_loop = %v, want conn.execute as written", got)
	} else if slices.Contains(got, "conn.describe") {
		t.Errorf("backfill calls_in_loop = %v: only a method that is I/O by its name is recorded unresolved", got)
	} else if slices.Contains(got, "conn.fetchall") || slices.Contains(got, "sys.stdout.flush") {
		t.Errorf("backfill calls_in_loop = %v: a cursor drain and a standard stream are not round trips", got)
	}
	if got := pyStrings(backfill.PropAny("calls_in_scaling_loop")); !slices.Contains(got, "conn.execute") {
		t.Errorf("backfill calls_in_scaling_loop = %v, want conn.execute", got)
	}
	// The local shares its name with a package of the repository, and is not it.
	if pyCallsTo(backfill, "conn.execute") || pyCallsTo(backfill, "conn/__init__.execute") {
		t.Errorf("the written call must make no edge; relations: %v", backfill.Relations)
	}
	if backfill.PropAny(propCallsAsWritten) != nil {
		t.Error("calls_as_written is the resolver's and must not reach the facts")
	}

	if got := pyStrings(pySym(t, ff, "migrate.once").PropAny("calls_in_loop")); len(got) != 0 {
		t.Errorf("once has no loop; calls_in_loop = %v", got)
	}

	drain := pyStrings(pySym(t, ff, "Loader.drain").PropAny("calls_in_loop"))
	if !slices.Contains(drain, "self.cursor.executemany") {
		t.Errorf("drain calls_in_loop = %v, want self.cursor.executemany", drain)
	}
	if len(drain) != 1 {
		t.Errorf("drain calls_in_loop = %v: a call on a call's result has no name to record", drain)
	}
}
