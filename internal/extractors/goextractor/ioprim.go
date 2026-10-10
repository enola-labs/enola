package goextractor

import (
	"go/ast"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// This file says which calls are I/O in themselves: the entry points of the
// standard library's network, file and database packages, and of the common
// drivers and clients. A function whose body makes one is io_direct, and
// ioclosure.Propagate carries that to everything that reaches it.
//
// The list is by package MEMBER, not by package. A package is too coarse a unit
// in both directions: `net/http` holds Client.Do and also ResponseWriter.Header,
// which returns a map, and `xorm` holds Session.Find and also Session.Where, which
// adds a clause to a query nobody has run. So a builder is absent on purpose, and
// so is everything that only might be I/O depending on what is behind it: io.Copy
// and io.ReadAll read whatever Reader they are handed, a network body or a byte
// slice, and say nothing either way.
//
// A call is matched by the target call resolution gave it, so it is matched only
// where the receiver's type was known. `client.Do(req)` on a value of unknown type
// is not found here. That is the intended failure: the name gate in the analyzer
// still reads it, and says it is a name match.

// goIOMembers maps a package's import path to its members that perform I/O: a
// function (`ReadFile`) or a method (`Client.Do`). `Type.*` is every method of
// the type but those in goIONonMembers. Import paths are written without a major
// version (`github.com/redis/go-redis`, for /v8 and /v9 alike).
var goIOMembers = map[string][]string{
	"net/http": {
		"Get", "Head", "Post", "PostForm",
		"Client.Do", "Client.Get", "Client.Head", "Client.Post", "Client.PostForm",
		"RoundTripper.RoundTrip", "Transport.RoundTrip",
	},
	"net": {
		"Dial", "DialTimeout", "DialTCP", "DialUDP", "DialUnix", "Listen",
		"Dialer.Dial", "Dialer.DialContext",
		"LookupHost", "LookupIP", "LookupAddr", "LookupCNAME", "LookupMX", "LookupTXT", "LookupSRV",
		"Resolver.LookupHost", "Resolver.LookupIPAddr", "Resolver.LookupAddr",
		"Conn.Read", "Conn.Write",
	},
	"net/smtp":   {"SendMail"},
	"crypto/tls": {"Dial", "DialWithDialer"},
	"database/sql": {
		"DB.Query", "DB.QueryContext", "DB.QueryRow", "DB.QueryRowContext", "DB.Exec", "DB.ExecContext",
		"DB.Prepare", "DB.PrepareContext", "DB.Begin", "DB.BeginTx", "DB.Ping", "DB.PingContext",
		"Tx.Query", "Tx.QueryContext", "Tx.QueryRow", "Tx.QueryRowContext", "Tx.Exec", "Tx.ExecContext",
		"Tx.Prepare", "Tx.PrepareContext", "Tx.Commit", "Tx.Rollback",
		"Conn.QueryContext", "Conn.QueryRowContext", "Conn.ExecContext", "Conn.PrepareContext",
		"Conn.BeginTx", "Conn.PingContext",
		"Stmt.Query", "Stmt.QueryContext", "Stmt.QueryRow", "Stmt.QueryRowContext", "Stmt.Exec", "Stmt.ExecContext",
	},
	"os": {
		"ReadFile", "WriteFile", "Open", "OpenFile", "Create", "CreateTemp", "Stat", "Lstat", "ReadDir",
		"Remove", "RemoveAll", "Rename", "Mkdir", "MkdirAll", "MkdirTemp", "Readlink", "Symlink", "Link",
		"Chmod", "Chown", "Chtimes", "Truncate",
		"File.Sync", "File.Readdir", "File.ReadDir", "File.Stat",
	},
	"io/ioutil":     {"ReadFile", "WriteFile", "ReadDir", "TempFile", "TempDir"},
	"path/filepath": {"Walk", "WalkDir", "Glob"},
	"os/exec":       {"Cmd.Run", "Cmd.Output", "Cmd.CombinedOutput", "Cmd.Start", "Cmd.Wait"},

	"xorm.io/xorm": {
		"Session.Get", "Session.Find", "Session.FindAndCount", "Session.Count", "Session.Exist",
		"Session.Insert", "Session.InsertOne", "Session.InsertMulti", "Session.Update", "Session.Delete",
		"Session.Truncate", "Session.Exec", "Session.Query", "Session.QueryString", "Session.QueryInterface",
		"Session.QuerySliceString", "Session.Iterate", "Session.Rows", "Session.Sum", "Session.SumInt",
		"Session.Sums", "Session.SumsInt", "Session.Sync", "Session.Sync2", "Session.Ping", "Session.PingContext",
		"Session.Begin", "Session.Commit", "Session.Rollback", "Session.DropTable", "Session.CreateTable",
		"Engine.Get", "Engine.Find", "Engine.FindAndCount", "Engine.Count", "Engine.Exist",
		"Engine.Insert", "Engine.InsertOne", "Engine.Update", "Engine.Delete",
		"Engine.Exec", "Engine.Query", "Engine.QueryString", "Engine.QueryInterface",
		"Engine.Iterate", "Engine.Rows", "Engine.Sum", "Engine.SumInt", "Engine.Sums", "Engine.SumsInt",
		"Engine.Sync", "Engine.Sync2", "Engine.Ping", "Engine.PingContext", "Engine.Transaction",
		"Engine.DropTables", "Engine.CreateTables",
	},
	"gorm.io/gorm": {
		"DB.First", "DB.Take", "DB.Last", "DB.Find", "DB.FindInBatches", "DB.FirstOrCreate",
		"DB.Create", "DB.CreateInBatches", "DB.Save", "DB.Update", "DB.Updates", "DB.UpdateColumn",
		"DB.UpdateColumns", "DB.Delete", "DB.Count", "DB.Scan", "DB.Row", "DB.Rows", "DB.Pluck", "DB.Exec",
		"DB.Transaction", "DB.Begin", "DB.Commit", "DB.Rollback",
	},
	"github.com/jmoiron/sqlx": {
		"DB.Get", "DB.GetContext", "DB.Select", "DB.SelectContext", "DB.Queryx", "DB.QueryxContext",
		"DB.QueryRowx", "DB.QueryRowxContext", "DB.NamedExec", "DB.NamedExecContext", "DB.NamedQuery",
		"DB.MustExec", "DB.Exec", "DB.ExecContext", "DB.Query", "DB.QueryContext", "DB.QueryRow",
		"DB.QueryRowContext", "DB.Beginx", "DB.BeginTxx", "DB.MustBegin",
		"Tx.Get", "Tx.GetContext", "Tx.Select", "Tx.SelectContext", "Tx.Queryx", "Tx.QueryxContext",
		"Tx.QueryRowx", "Tx.QueryRowxContext", "Tx.NamedExec", "Tx.NamedExecContext", "Tx.NamedQuery",
		"Tx.MustExec", "Tx.Exec", "Tx.ExecContext", "Tx.Query", "Tx.QueryContext", "Tx.QueryRow",
		"Tx.QueryRowContext", "Tx.Commit", "Tx.Rollback",
	},
	"github.com/jackc/pgx": {
		"Conn.Query", "Conn.QueryRow", "Conn.Exec", "Conn.Begin", "Conn.BeginTx", "Conn.SendBatch", "Conn.CopyFrom",
		"Tx.Query", "Tx.QueryRow", "Tx.Exec", "Tx.Begin", "Tx.SendBatch", "Tx.CopyFrom", "Tx.Commit", "Tx.Rollback",
	},
	"github.com/jackc/pgx/pgxpool": {
		"Pool.Query", "Pool.QueryRow", "Pool.Exec", "Pool.Begin", "Pool.BeginTx", "Pool.SendBatch",
		"Pool.CopyFrom", "Pool.Acquire",
	},
	"github.com/redis/go-redis":               {"Client.*", "ClusterClient.*", "Ring.*"},
	"github.com/go-redis/redis":               {"Client.*", "ClusterClient.*", "Ring.*"},
	"github.com/bradfitz/gomemcache/memcache": {"Client.*"},
	"go.mongodb.org/mongo-driver/mongo": {
		"Collection.*", "Cursor.All",
		"Database.RunCommand", "Database.ListCollectionNames", "Database.Drop",
		"Client.Connect", "Client.Ping", "Client.Disconnect",
	},
	"google.golang.org/grpc": {
		"ClientConnInterface.Invoke", "ClientConnInterface.NewStream",
		"ClientConn.Invoke", "ClientConn.NewStream", "Dial", "DialContext",
	},
	"github.com/go-resty/resty": {
		"Request.Get", "Request.Post", "Request.Put", "Request.Delete", "Request.Patch", "Request.Head",
		"Request.Options", "Request.Execute", "Request.Send",
	},
	"github.com/minio/minio-go":     {"Client.*"},
	"github.com/segmentio/kafka-go": {"Writer.WriteMessages", "Reader.ReadMessage", "Reader.FetchMessage", "Reader.CommitMessages"},
	"github.com/IBM/sarama":         {"SyncProducer.SendMessage", "SyncProducer.SendMessages"},
	"github.com/Shopify/sarama":     {"SyncProducer.SendMessage", "SyncProducer.SendMessages"},
}

// goIOPackagePrefixes are trees of packages that each hold one generated client
// whose methods are all remote calls: `service/s3.Client.GetObject`.
var goIOPackagePrefixes = map[string]*goIOEntry{
	"github.com/aws/aws-sdk-go-v2/service/": newGoIOEntry([]string{"Client.*"}),
}

// goIONonMembers are methods a `Type.*` entry does not cover: the ones that hand
// back a handle, a builder or a setting and send nothing.
var goIONonMembers = map[string]bool{
	"Options": true, "String": true, "Context": true, "WithContext": true, "WithTimeout": true,
	"AddHook": true, "PoolStats": true, "Conn": true, "Pipeline": true, "TxPipeline": true,
	"Name": true, "Database": true, "Clone": true, "Indexes": true, "EndpointURL": true,
}

var goMajorVersion = regexp.MustCompile(`/v[0-9]+(/|$)`)

var goIOIndex = buildGoIOIndex()

type goIOEntry struct {
	exact map[string]bool // "Client.Do", "ReadFile"
	types map[string]bool // "Client" for "Client.*"
}

func buildGoIOIndex() map[string]*goIOEntry {
	idx := make(map[string]*goIOEntry, len(goIOMembers))
	for pkg, members := range goIOMembers {
		idx[pkg] = newGoIOEntry(members)
	}
	return idx
}

func newGoIOEntry(members []string) *goIOEntry {
	e := &goIOEntry{exact: make(map[string]bool, len(members)), types: make(map[string]bool)}
	for _, m := range members {
		if t, ok := strings.CutSuffix(m, ".*"); ok {
			e.types[t] = true
		} else {
			e.exact[m] = true
		}
	}
	return e
}

func (e *goIOEntry) has(member string) bool {
	if e.exact[member] {
		return true
	}
	t, method, ok := strings.Cut(member, ".")
	return ok && e.types[t] && !goIONonMembers[method]
}

// goIOPrimitive reports whether a resolved call target is one of the I/O entry
// points: `net/http.Client.Do`, `os.ReadFile`, `xorm.io/xorm.Session.Find`.
func goIOPrimitive(target string) bool {
	pkg, member := splitGoTarget(target)
	if member == "" {
		return false
	}
	pkg = strings.TrimSuffix(goMajorVersion.ReplaceAllString(pkg, "$1"), "/")
	if e := goIOIndex[pkg]; e != nil {
		return e.has(member)
	}
	for prefix, e := range goIOPackagePrefixes {
		if strings.HasPrefix(pkg, prefix) && e.has(member) {
			return true
		}
	}
	return false
}

// splitGoTarget splits a call target into its package's import path and the
// member called: `xorm.io/xorm.Session.Find` is `xorm.io/xorm` and `Session.Find`.
// The package name is the last path element up to its first dot, which is where
// an import path with dots in its host part differs from a plain split.
func splitGoTarget(target string) (pkg, member string) {
	slash := strings.LastIndexByte(target, '/')
	dot := strings.IndexByte(target[slash+1:], '.')
	if dot < 0 {
		return target, ""
	}
	dot += slash + 1
	return target[:dot], target[dot+1:]
}

// goIOCall reports whether a resolved call target performs I/O in itself: it is
// an entry point, or it is one reached under another type's name. There are two
// ways a project gives an entry point its own name, and both are statements in
// the source, not guesses:
//
//	type Engine interface { Find(...) error; Where(...) *xorm.Session }
//	var _ Engine = (*xorm.Session)(nil)
//
// makes `Engine.Find` the call `xorm.Session.Find`, and leaves `Engine.Where`
// the builder it is. And
//
//	type DBSession struct { *xorm.Session }
//
// promotes the session's methods, so `DBSession.Exec` is `xorm.Session.Exec`.
func goIOCall(target string, types callTypeTables) bool {
	if goIOPrimitive(target) {
		return true
	}
	dot := strings.LastIndexByte(target, '.')
	if dot < 0 {
		return false
	}
	owner, member := target[:dot], target[dot:]
	for _, t := range types.implementers[owner] {
		if goIOPrimitive(t + member) {
			return true
		}
	}
	for _, t := range types.embeds[owner] {
		if goIOPrimitive(t + member) {
			return true
		}
	}
	return false
}

// markGoDirectIO records on a function's fact the I/O calls its body makes.
// io_calls names them, so a consumer can tell the call that is the I/O from the
// others in the same loop.
func markGoDirectIO(f *facts.Fact, types callTypeTables, calls ...[]string) {
	seen := make(map[string]bool)
	for _, list := range calls {
		for _, c := range list {
			if goIOCall(c, types) {
				seen[c] = true
			}
		}
	}
	if len(seen) == 0 {
		return
	}
	prims := make([]string, 0, len(seen))
	for c := range seen {
		prims = append(prims, c)
	}
	sort.Strings(prims)
	f.SetProp("io_direct", true)
	f.SetProp("io_calls", prims)
}

// goObservabilityPackages are the package names whose functions report on the
// program and are not its work: logging, tracing, metrics.
var goObservabilityPackages = map[string]bool{
	"log": true, "logs": true, "logger": true, "logging": true, "slog": true,
	"trace": true, "tracing": true, "tracer": true,
	"metrics": true, "telemetry": true, "instrumentation": true,
}

// goObservability reports whether a function sits in a logging, tracing or
// metrics package of the module, by the last element of its package path.
//
// Such a function does reach I/O: a log line ends at a file, a span at a
// collector. Carrying that to its callers makes "performs I/O" true of every
// function that logs, which on one repository was 45 in 100, and says nothing
// about any of them. So the flag stops there. A function that writes the log
// file is still io_direct; it is the handler that called log.Error that is not.
func goObservability(name string) bool {
	pkg, _ := splitGoTarget(name)
	if i := strings.LastIndexByte(pkg, '/'); i >= 0 {
		pkg = pkg[i+1:]
	}
	return goObservabilityPackages[pkg]
}

// isOnceDo reports whether a call runs its function argument once per process:
// `once.Do(func() { … })` on a sync.Once. Where the receiver's type did not
// resolve, a receiver named for what it is (`initOnce`, `c.once`) is accepted.
//
// It matters here because lazy initialisation is the other way "performs I/O"
// becomes true of everything: the function every query goes through to get its
// engine pings the database the first time it is called, and so every caller of
// every query would do I/O through it.
func isOnceDo(call *ast.CallExpr, resolved string) bool {
	if len(call.Args) != 1 || !strings.HasSuffix(resolved, ".Do") {
		return false
	}
	if _, ok := call.Args[0].(*ast.FuncLit); !ok {
		return false
	}
	return resolved == "sync.Once.Do" || strings.Contains(strings.ToLower(resolved), "once")
}

// onceOnly returns the calls of a body that are made only inside a once.Do.
func onceOnly(calls, perCall []string) []string {
	if len(calls) == len(perCall) {
		return nil
	}
	repeated := make(map[string]bool, len(perCall))
	for _, c := range perCall {
		repeated[c] = true
	}
	var out []string
	for _, c := range calls {
		if !repeated[c] {
			out = append(out, c)
		}
	}
	return out
}

// goFluentTypes are the query-builder types of another module whose methods,
// other than the ones that run the query, return the builder: the type on the
// left, and what its builder methods return on the right.
//
// A method of another module has no declared result here, so a chain through
// one stopped resolving at its first link: in `x.Cols("scope").ID(id).Update(t)`
// the Update was a call on a value of unknown type, and the one call in the
// statement that reaches the database was the one not recorded.
var goFluentTypes = map[string]string{
	"xorm.io/xorm.Session": "xorm.io/xorm.Session",
	"xorm.io/xorm.Engine":  "xorm.io/xorm.Session",
	"gorm.io/gorm.DB":      "gorm.io/gorm.DB",
}

// goFluentResult returns the type a builder method of a fluent type returns, or
// "" when target is not one. A method that runs the query is not a builder.
func goFluentResult(target string) string {
	dot := strings.LastIndexByte(target, '.')
	if dot < 0 {
		return ""
	}
	result, ok := goFluentTypes[target[:dot]]
	if !ok || goIOPrimitive(target) {
		return ""
	}
	return result
}
