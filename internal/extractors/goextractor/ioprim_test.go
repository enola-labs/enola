package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestGoIOPrimitive_IsByMemberNotByPackage(t *testing.T) {
	for target, want := range map[string]bool{
		"net/http.Client.Do":                                       true,
		"net/http.Get":                                             true,
		"net/http.ResponseWriter.Header":                           false, // returns a map
		"net/http.NewRequest":                                      false, // builds
		"os.ReadFile":                                              true,
		"os.Getenv":                                                false,
		"database/sql.DB.QueryContext":                             true,
		"database/sql.Rows.Next":                                   false, // draining a cursor is not a round trip
		"xorm.io/xorm.Session.Find":                                true,
		"xorm.io/xorm.Session.Where":                               false, // adds a clause
		"gorm.io/gorm.DB.Where":                                    false,
		"gorm.io/gorm.DB.First":                                    true,
		"github.com/redis/go-redis/v9.Client.Get":                  true, // a major version is not part of the name
		"github.com/redis/go-redis/v9.Client.Pipeline":             false,
		"github.com/jackc/pgx/v5/pgxpool.Pool.Query":               true,
		"github.com/aws/aws-sdk-go-v2/service/s3.Client.GetObject": true,
		"io.ReadAll":                                               false, // reads whatever it is handed
		"strings.Builder.String":                                   false,
		"pkg/store.load":                                           false,
	} {
		if got := goIOPrimitive(target); got != want {
			t.Errorf("goIOPrimitive(%s) = %v, want %v", target, got, want)
		}
	}
}

// extractGoModule writes files as a module and returns its symbol facts by name.
func extractGoModule(t *testing.T, files map[string]string) map[string]facts.Fact {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/app\n\ngo 1.22\n"
	var rel []string
	for name, src := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(name) == ".go" {
			rel = append(rel, name)
		}
	}
	slices.Sort(rel)
	ff, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]facts.Fact)
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			out[f.Name] = f
		}
	}
	return out
}

func goPerformsIO(f facts.Fact) bool {
	b, _ := f.PropAny("performs_io").(bool)
	return b
}

func goIOCalls(f facts.Fact) []string {
	s, _ := f.PropAny("io_calls").([]string)
	return s
}

func TestGoPerformsIO_FromAPrimitiveThroughWrappers(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"fetch/fetch.go": `package fetch

import "net/http"

func One(c *http.Client, req *http.Request) (*http.Response, error) { return c.Do(req) }

func All(c *http.Client, reqs []*http.Request) {
	for _, r := range reqs {
		One(c, r)
	}
}

func Header(w http.ResponseWriter) string { return w.Header().Get("X") }
`,
	})
	one := syms["fetch.One"]
	if !goPerformsIO(one) || !slices.Equal(goIOCalls(one), []string{"net/http.Client.Do"}) {
		t.Errorf("One: performs_io=%v io_calls=%v, want the Do call", goPerformsIO(one), goIOCalls(one))
	}
	if all := syms["fetch.All"]; !goPerformsIO(all) || len(goIOCalls(all)) != 0 {
		t.Errorf("All: performs_io=%v io_calls=%v, want it through One and no call of its own", goPerformsIO(all), goIOCalls(all))
	}
	if goPerformsIO(syms["fetch.Header"]) {
		t.Errorf("Header reads a response header map and is flagged")
	}
}

// A project's own interface over its database handle, tied to the handle by an
// assertion. A method the handle runs a query in is I/O under the interface's
// name, and a builder is not.
func TestGoPerformsIO_ThroughAnAssertedFacade(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"db/engine.go": `package db

import "xorm.io/xorm"

type SQLSession interface {
	Find(beans ...any) error
	Where(query any, args ...any) *xorm.Session
}

type Engine interface {
	SQLSession
	Ping() error
}

var _ Engine = (*xorm.Session)(nil)
`,
		"repo/repo.go": `package repo

import "example.com/app/db"

func Load(e db.Engine, out any) error { return e.Find(out) }

func Filter(e db.Engine) any { return e.Where("id > ?", 1) }

func Newest(e db.Engine, out any) error { return e.Where("id > ?", 1).Desc("id").Find(out) }
`,
	})
	if f := syms["repo.Load"]; !goPerformsIO(f) {
		t.Errorf("Load: a Find on the facade is not flagged; relations=%v", f.Relations)
	}
	if f := syms["repo.Filter"]; goPerformsIO(f) {
		t.Errorf("Filter only builds a query and is flagged: io_calls=%v", goIOCalls(f))
	}
	// Where returns the library's session; Desc is one of its builders; Find runs.
	if f := syms["repo.Newest"]; !slices.Contains(goIOCalls(f), "xorm.io/xorm.Session.Find") {
		t.Errorf("Newest: io_calls=%v, want the Find at the end of the chain", goIOCalls(f))
	}
}

// A struct that embeds a type of this module promotes its methods. The call is
// recorded against the method that is declared, so the edge lands somewhere.
func TestGoPromotedMethodCallResolvesToItsDeclaration(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"orm/session.go": `package orm

import "database/sql"

type Session struct{ db *sql.DB }

func (s *Session) Exec(q string) error { _, err := s.db.Exec(q); return err }
`,
		"store/store.go": `package store

import "example.com/app/orm"

type DBSession struct {
	*orm.Session
	open bool
}

func Run(sess *DBSession, qs []string) {
	for _, q := range qs {
		sess.Exec(q)
	}
}
`,
	})
	run := syms["store.Run"]
	var targets []string
	for _, r := range run.Relations {
		if r.Kind == facts.RelCalls {
			targets = append(targets, r.Target)
		}
	}
	if !slices.Contains(targets, "orm.Session.Exec") {
		t.Errorf("Run calls %v, want orm.Session.Exec", targets)
	}
	if inLoop, _ := run.PropAny("calls_in_loop").([]string); !slices.Contains(inLoop, "orm.Session.Exec") {
		t.Errorf("Run calls_in_loop = %v, want it renamed with the edge", inLoop)
	}
	if !goPerformsIO(run) {
		t.Errorf("Run is not flagged: the promoted Exec reaches database/sql")
	}
}

// What a function does once per process is not what it does per call, and a
// logger's file is not its caller's I/O.
func TestGoPerformsIO_StopsAtOnceAndAtLogging(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"conf/conf.go": `package conf

import (
	"os"
	"sync"
)

var (
	once sync.Once
	data []byte
)

func Get() []byte {
	once.Do(func() { data, _ = os.ReadFile("app.ini") })
	return data
}

func Reload() []byte { data, _ = os.ReadFile("app.ini"); return data }
`,
		"log/log.go": `package log

import "os"

func Error(msg string) { f, _ := os.OpenFile("app.log", os.O_APPEND, 0o644); f.WriteString(msg) }
`,
		"app/app.go": `package app

import (
	"example.com/app/conf"
	"example.com/app/log"
)

func Size() int { return len(conf.Get()) }

func Fresh() int { return len(conf.Reload()) }

func Check(n int) {
	if n < 0 {
		log.Error("negative")
	}
}
`,
	})
	if goPerformsIO(syms["conf.Get"]) || goPerformsIO(syms["app.Size"]) {
		t.Errorf("a read inside once.Do is carried to the caller: Get=%v Size=%v", goPerformsIO(syms["conf.Get"]), goPerformsIO(syms["app.Size"]))
	}
	if !goPerformsIO(syms["app.Fresh"]) {
		t.Errorf("Fresh reads the file on every call and is not flagged")
	}
	if !goPerformsIO(syms["log.Error"]) {
		t.Errorf("log.Error opens a file and is not itself flagged")
	}
	if goPerformsIO(syms["app.Check"]) {
		t.Errorf("Check is flagged for logging")
	}
}
