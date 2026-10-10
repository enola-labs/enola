package pythonextractor

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// This file says what a call into a library is, where call resolution named the
// library member: `sqlalchemy.orm.Session.execute`, not a method called `execute`.
// It is the Python counterpart of goextractor/ioprim.go and follows the same rule:
// the list is by MEMBER. `Session.execute` is a round trip and `Session.delete`
// marks an object for the next flush; `requests.get` is a request and
// `requests.cookies.RequestsCookieJar.set` fills a dict.
//
// A call is matched by the target resolution gave it, so only where the receiver's
// type was known. `client.execute(q)` on a value of unknown type is not found
// here, and the name lists in python_ast.go still read it.

// pyLibrary describes one library, or one type of one.
type pyLibrary struct {
	// io are the members that perform I/O: a function (`get`), a function of a
	// submodule (`path.exists`), a method (`Session.execute`) or every method of
	// a type (`Inspector.*`).
	io []string
	// neutral are the members that run or read what they are handed and say
	// nothing either way: `asyncio.gather` awaits whatever it was given.
	neutral []string
	// open says the io list is not the whole of the library's I/O, so a member
	// that is not on it is unknown. Without it the list is complete: a member
	// that is not on it does no I/O.
	open bool
}

// pyLibraries is keyed by a dotted prefix of the call target: a distribution's
// import root (`sqlalchemy`) or one of its types (`io.StringIO`). The longest
// matching prefix decides.
var pyLibraries = map[string]pyLibrary{
	"sqlalchemy": {io: []string{
		"Session.execute", "Session.scalar", "Session.scalars", "Session.get", "Session.get_one",
		"Session.merge", "Session.flush", "Session.commit", "Session.rollback", "Session.refresh",
		"Session.bulk_save_objects", "Session.bulk_insert_mappings", "Session.bulk_update_mappings",
		"Session.connection",
		"AsyncSession.execute", "AsyncSession.scalar", "AsyncSession.scalars", "AsyncSession.get",
		"AsyncSession.get_one", "AsyncSession.merge", "AsyncSession.flush", "AsyncSession.commit",
		"AsyncSession.rollback", "AsyncSession.refresh", "AsyncSession.stream", "AsyncSession.stream_scalars",
		"scoped_session.execute", "scoped_session.scalar", "scoped_session.scalars", "scoped_session.get",
		"scoped_session.merge", "scoped_session.flush", "scoped_session.commit", "scoped_session.rollback",
		"scoped_session.refresh", "scoped_session.bulk_save_objects", "scoped_session.bulk_insert_mappings",
		"scoped_session.bulk_update_mappings",
		"Connection.execute", "Connection.scalar", "Connection.scalars", "Connection.exec_driver_sql",
		"Connection.commit", "Connection.rollback",
		"AsyncConnection.execute", "AsyncConnection.scalar", "AsyncConnection.scalars",
		"AsyncConnection.exec_driver_sql", "AsyncConnection.commit", "AsyncConnection.rollback",
		"AsyncConnection.stream",
		"Engine.connect", "Engine.begin", "Engine.execute", "AsyncEngine.connect", "AsyncEngine.begin",
		"Query.all", "Query.first", "Query.one", "Query.one_or_none", "Query.scalar", "Query.count",
		"Query.get", "Query.delete", "Query.update",
		"Inspector.*",
		"MetaData.reflect", "MetaData.create_all", "MetaData.drop_all", "Table.create", "Table.drop",
	},
		// `session.query(M)` builds a Query, and the call that runs it is made on
		// that result (`.filter_by(…).one_or_none()`), which nothing here types. The
		// builder is the only part of the chain a finding can name, so it is left
		// to the name lists and not called pure.
		neutral: []string{"Session.query", "scoped_session.query"},
	},
	"requests": {io: []string{
		"get", "post", "put", "patch", "delete", "head", "options", "request",
		"Session.get", "Session.post", "Session.put", "Session.patch", "Session.delete", "Session.head",
		"Session.options", "Session.request", "Session.send",
	}},
	"httpx": {io: []string{
		"get", "post", "put", "patch", "delete", "head", "options", "request", "stream",
		"Client.get", "Client.post", "Client.put", "Client.patch", "Client.delete", "Client.head",
		"Client.options", "Client.request", "Client.send", "Client.stream",
		"AsyncClient.get", "AsyncClient.post", "AsyncClient.put", "AsyncClient.patch", "AsyncClient.delete",
		"AsyncClient.head", "AsyncClient.options", "AsyncClient.request", "AsyncClient.send", "AsyncClient.stream",
	}},
	"aiohttp": {io: []string{
		"request",
		"ClientSession.get", "ClientSession.post", "ClientSession.put", "ClientSession.patch",
		"ClientSession.delete", "ClientSession.head", "ClientSession.options", "ClientSession.request",
		"ClientSession.ws_connect",
	}},
	"urllib": {io: []string{"urlopen", "urlretrieve"}},
	"subprocess": {io: []string{
		"run", "call", "check_call", "check_output", "getoutput", "getstatusoutput", "Popen",
		"Popen.communicate", "Popen.wait",
	}},
	"os": {io: []string{
		"open", "read", "write", "fsync", "remove", "unlink", "rename", "replace", "mkdir", "makedirs",
		"rmdir", "removedirs", "listdir", "scandir", "walk", "stat", "lstat", "chmod", "chown", "symlink",
		"link", "readlink", "truncate", "utime", "system", "popen",
		"path.exists", "path.lexists", "path.isfile", "path.isdir", "path.islink", "path.getsize",
		"path.getmtime", "path.getatime", "path.getctime", "path.samefile",
	}},
	"shutil": {io: []string{
		"copy", "copy2", "copyfile", "copyfileobj", "copymode", "copystat", "copytree", "rmtree", "move",
		"disk_usage", "chown", "make_archive", "unpack_archive",
	}},
	"pathlib": {io: []string{
		"Path.exists", "Path.is_file", "Path.is_dir", "Path.is_symlink", "Path.stat", "Path.lstat",
		"Path.read_text", "Path.read_bytes", "Path.write_text", "Path.write_bytes", "Path.open",
		"Path.mkdir", "Path.rmdir", "Path.unlink", "Path.rename", "Path.replace", "Path.touch",
		"Path.iterdir", "Path.glob", "Path.rglob", "Path.walk", "Path.chmod", "Path.symlink_to",
		"Path.hardlink_to", "Path.samefile",
	}},
	"smtplib": {io: []string{"SMTP.*", "SMTP_SSL.*", "LMTP.*"}},
	"asyncio": {
		io: []string{
			"open_connection", "open_unix_connection", "start_server", "start_unix_server",
			"create_subprocess_exec", "create_subprocess_shell",
		},
		// These run the coroutine or the function they are given.
		neutral: []string{
			"run", "gather", "wait", "wait_for", "as_completed", "shield", "to_thread", "create_task",
			"ensure_future", "timeout", "run_coroutine_threadsafe",
			"AbstractEventLoop.run_in_executor", "AbstractEventLoop.run_until_complete", "TaskGroup.create_task",
		},
	},
	"pandas": {io: []string{
		"read_csv", "read_sql", "read_sql_query", "read_sql_table", "read_parquet", "read_json",
		"read_excel", "read_pickle", "read_feather", "read_orc", "read_gbq", "read_html", "read_xml",
		"DataFrame.to_sql", "DataFrame.to_csv", "DataFrame.to_parquet", "DataFrame.to_excel",
		"DataFrame.to_pickle", "DataFrame.to_feather", "DataFrame.to_gbq",
	}},
	"numpy": {io: []string{"load", "save", "savez", "savez_compressed", "loadtxt", "savetxt", "genfromtxt", "fromfile"}},
	// A migration's operations are statements against the schema. The module has
	// more of them than are worth listing.
	"alembic": {open: true, io: []string{
		"execute", "bulk_insert", "add_column", "drop_column", "alter_column", "create_table", "drop_table",
		"rename_table", "create_index", "drop_index", "create_foreign_key", "create_unique_constraint",
		"create_check_constraint", "create_primary_key", "drop_constraint",
	}},

	// Libraries and types that work on memory and nothing else.
	"re":          {},
	"logging":     {},
	"structlog":   {},
	"collections": {},
	"itertools":   {},
	"functools":   {},
	"operator":    {},
	"math":        {},
	"string":      {},
	"textwrap":    {},
	"datetime":    {},
	"enum":        {},
	"dataclasses": {},
	"typing":      {},
	"hashlib":     {},
	"base64":      {},
	"uuid":        {},
	"ast":         {},
	"jmespath":    {},
	"io.StringIO": {},
	"io.BytesIO":  {},
}

// What pyLibraryCall says of a call target.
const (
	pyCallUnknown = iota // not a member of a library this file describes, or one it is silent on
	pyCallIO             // performs I/O
	pyCallPure           // a member of a described library that performs none
)

// pyLibraryCall classes a dotted call target that resolution left outside the
// repository.
func pyLibraryCall(target string) int {
	lib, rest, ok := pyLibraryOf(target)
	if !ok {
		return pyCallUnknown
	}
	names := pyMemberNames(rest)
	if pyHasMember(lib.io, names) {
		return pyCallIO
	}
	if lib.open || pyHasMember(lib.neutral, names) {
		return pyCallUnknown
	}
	return pyCallPure
}

// pyLibraryOf finds the entry with the longest prefix of target, and what of
// target follows it.
func pyLibraryOf(target string) (lib pyLibrary, rest string, ok bool) {
	for prefix := target; ; {
		i := strings.LastIndexByte(prefix, '.')
		if i < 0 {
			return pyLibrary{}, "", false
		}
		prefix = prefix[:i]
		if lib, ok := pyLibraries[prefix]; ok {
			return lib, target[len(prefix)+1:], true
		}
	}
}

// pyMemberNames returns the names a member may be listed under, given the part
// of its target that follows the library: `Session.execute` and `Session.*` for
// `orm.session.Session.execute`, `path.exists` and `exists` for `path.exists`.
// A method is never matched by its bare name, or `Session.delete` would be the
// `delete` function of the same library.
func pyMemberNames(rest string) []string {
	segs := strings.Split(rest, ".")
	last := segs[len(segs)-1]
	if len(segs) == 1 {
		return []string{last}
	}
	owner := segs[len(segs)-2]
	if pyCapitalized(owner) {
		return []string{owner + "." + last, owner + ".*"}
	}
	return []string{owner + "." + last, last}
}

func pyHasMember(members, names []string) bool {
	for _, m := range members {
		for _, n := range names {
			if m == n {
				return true
			}
		}
	}
	return false
}

// pyLibraryCalls collects, for one function, the library calls its body makes.
type pyLibraryCalls struct {
	io, pure map[string]bool
}

// note classes one call target that resolution left outside the repository.
// inLoop says the target came from one of the in-loop call lists: a call that
// does no I/O is worth recording only there, where its name would be read.
func (c *pyLibraryCalls) note(target string, inLoop bool) {
	switch pyLibraryCall(target) {
	case pyCallIO:
		if c.io == nil {
			c.io = make(map[string]bool)
		}
		c.io[target] = true
	case pyCallPure:
		if !inLoop {
			return
		}
		if c.pure == nil {
			c.pure = make(map[string]bool)
		}
		c.pure[target] = true
	}
}

// mark records the calls on the function's fact. io_calls names the calls that
// are the I/O, so a consumer can tell them from the others in the same loop, and
// makes the function io_direct. pure_calls names the in-loop calls into a
// described library that are none, so a consumer does not read them by their names.
func (c *pyLibraryCalls) mark(f *facts.Fact) {
	if len(c.io) > 0 {
		f.SetProp("io_direct", true)
		f.SetProp("io_calls", sortedKeys(c.io))
	}
	if len(c.pure) > 0 {
		f.SetProp("pure_calls", sortedKeys(c.pure))
	}
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
