package scalaextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func routeFacts(ff []facts.Fact) map[string]facts.Fact {
	out := map[string]facts.Fact{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute {
			continue
		}
		m, _ := f.Props["method"].(string)
		out[m+" "+f.Name] = f
	}
	return out
}

func routeKeys(ff []facts.Fact) []string {
	var out []string
	for k := range routeFacts(ff) {
		out = append(out, k)
	}
	return out
}

func extractRoutes(t *testing.T, relFile, src string) []facts.Fact {
	t.Helper()
	return extractDSLRoutes([]byte(src), relFile)
}

// --- Play ---

// writePlayRepo materializes a conf/ directory and returns the repo root.
func writePlayRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPlayRoutesBasicForms(t *testing.T) {
	root := writePlayRepo(t, map[string]string{
		"conf/routes": `# a comment
GET     /                       controllers.Home.index
GET     /users/:id              controllers.Users.show(id: Long)
POST    /teams                  controllers.Teams.create
GET     /assets/*file           controllers.Assets.at(path="/public", file)
GET     /$lang<\w\w>/tv         controllers.Tv.indexLang(lang: Language)
+ nocsrf
DELETE  /teams/:id              controllers.Teams.delete(id: Long)
`,
	})
	got := routeFacts(extractPlayRoutes(root))

	for _, want := range []string{
		"GET /", "GET /users/:id", "POST /teams",
		"GET /assets/:file", // catch-all normalized
		"GET /:lang/tv",     // regex constraint dropped, parameter kept
		"DELETE /teams/:id",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing route %q; have %v", want, routeKeys(extractPlayRoutes(root)))
		}
	}
	// The handler is the symbol; the parameter list is type ascription, not identity.
	if h := got["GET /users/:id"].Props["handler"]; h != "controllers.Users.show" {
		t.Errorf("handler = %v, want controllers.Users.show", h)
	}
	if got["GET /"].Props[facts.PropSource] != facts.RouteSourcePlayRoutes {
		t.Errorf("source = %v, want %v", got["GET /"].Props[facts.PropSource], facts.RouteSourcePlayRoutes)
	}
}

// TestPlayIncludeComposesPrefix pins the mount composition — the reason a routes
// file is a tree rather than a list.
func TestPlayIncludeComposesPrefix(t *testing.T) {
	root := writePlayRepo(t, map[string]string{
		"conf/routes": `GET  /            controllers.Home.index
->   /admin       admin.Routes
`,
		"conf/admin.routes": `GET   /users      controllers.admin.Users.list
POST  /users/:id  controllers.admin.Users.update(id: Long)
`,
	})
	got := routeFacts(extractPlayRoutes(root))
	for _, want := range []string{"GET /admin/users", "POST /admin/users/:id"} {
		if _, ok := got[want]; !ok {
			t.Errorf("include not composed: missing %q; have %v", want, routeKeys(extractPlayRoutes(root)))
		}
	}
}

// TestPlayAbsoluteSubRoutesAreNotDoubled is the counterpart, and it is a measured
// case rather than a hypothetical: every included routes file in the benchmark
// corpus repeats its mount prefix on every line. Composing blindly produced
// `/team/team`, an endpoint the application does not serve.
func TestPlayAbsoluteSubRoutesAreNotDoubled(t *testing.T) {
	root := writePlayRepo(t, map[string]string{
		"conf/routes": `->   /team    team.Routes
`,
		// Absolute paths: the prefix is already there.
		"conf/team.routes": `GET   /team          controllers.team.Team.home
GET   /team/:id      controllers.team.Team.show(id: Long)
GET   /teams         controllers.team.Team.all
`,
	})
	got := routeFacts(extractPlayRoutes(root))

	if _, bad := got["GET /team/team"]; bad {
		t.Errorf("mount prefix doubled; have %v", routeKeys(extractPlayRoutes(root)))
	}
	for _, want := range []string{"GET /team", "GET /team/:id"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; have %v", want, routeKeys(extractPlayRoutes(root)))
		}
	}
	// The guard is segment-wise: `/teams` shares a string prefix with `/team` but
	// not a segment, so it still composes.
	if _, ok := got["GET /team/teams"]; !ok {
		t.Errorf("a route sharing only a string prefix must still compose; have %v",
			routeKeys(extractPlayRoutes(root)))
	}
}

func TestPlayNoConfDirectoryIsNotAPlayApp(t *testing.T) {
	root := writePlayRepo(t, map[string]string{"src/main/scala/A.scala": "package a\nclass A\n"})
	if ff := extractPlayRoutes(root); len(ff) != 0 {
		t.Errorf("emitted %d routes for a repo with no conf/", len(ff))
	}
}

// --- Pekko / Akka HTTP ---

func TestPekkoRouteForms(t *testing.T) {
	src := `package p

import org.apache.pekko.http.scaladsl.server.Directives._

object Server {
  val routes =
    pathPrefix("api") {
      path("users") {
        get { complete(OK) }
      } ~ (path("teams") & post) {
        complete(OK)
      } ~ pathPrefix("v2" / "admin") {
        path("ping") { complete(OK) }
      }
    }
}
`
	got := routeFacts(extractRoutes(t, "src/Server.scala", src))
	for _, want := range []string{
		"GET /api/users",       // nested verb directive
		"POST /api/teams",      // conjoined `& post`
		"* /api/v2/admin/ping", // composed multi-segment prefix, no verb
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; have %v", want, routeKeys(extractRoutes(t, "src/Server.scala", src)))
		}
	}
}

// TestPekkoRequiresTheServerImport pins the gate that killed a whole false-positive
// class. `path` is an ordinary method name, and a metrics helper whose API is
// `path(result).record(nanos)` produced four routes on a file that serves no HTTP.
func TestPekkoRequiresTheServerImport(t *testing.T) {
	src := `package p

/** A metrics timer, not a web server. */
object Chronometer {
  def record(result: Result) = path(result).record(nanos)
  def other(r: R) = path(r.isSuccess)
}
`
	if ff := extractRoutes(t, "src/Chronometer.scala", src); len(ff) != 0 {
		t.Errorf("emitted %d routes from a file that never imports the routing DSL: %v",
			len(ff), routeKeys(ff))
	}

	// The same shape WITH the import is a real route tree.
	withImport := "import org.apache.pekko.http.scaladsl.server.Directives._\n" + src
	if ff := extractRoutes(t, "src/S.scala", withImport); len(ff) == 0 {
		t.Error("the import gate suppressed a genuine route tree")
	}
}

// TestPekkoUnresolvedSegmentIsFlagged pins the honest-degradation case: a prefix
// held in a value cannot be resolved without evaluating Scala, so the route keeps
// what is known and says the rest is not.
func TestPekkoUnresolvedSegmentIsFlagged(t *testing.T) {
	src := `package p

import org.apache.pekko.http.scaladsl.server.Directives._

object S {
  val routes = pathPrefix(collection.path) { path("count") { get { complete(OK) } } }
}
`
	ff := extractRoutes(t, "src/S.scala", src)
	if len(ff) == 0 {
		t.Fatal("an unresolvable prefix dropped the route entirely")
	}
	found := false
	for _, f := range ff {
		if f.Props["path_unresolved"] == true {
			found = true
		}
	}
	if !found {
		t.Errorf("unresolved prefix not flagged: %+v", ff[0].Props)
	}
}

// --- http4s ---

func TestHTTP4sRouteForms(t *testing.T) {
	src := `package p

import org.http4s.HttpRoutes

object Api {
  val routes = HttpRoutes.of[IO] {
    case GET -> Root / "users" / LongVar(id) => Ok(id)
    case POST -> Root / "teams"              => Ok()
    case GET -> Root                          => Ok()
    case GET -> Root / "v1" / "ws"            => Ok()
  }
}
`
	got := routeFacts(extractRoutes(t, "src/Api.scala", src))
	for _, want := range []string{
		"GET /users/:id", // extractor variable becomes the canonical :name form
		"POST /teams",
		"GET /",
		"GET /v1/ws",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; have %v", want, routeKeys(extractRoutes(t, "src/Api.scala", src)))
		}
	}
	if got["POST /teams"].Props[facts.PropSource] != facts.RouteSourceHTTP4s {
		t.Errorf("source = %v, want %v", got["POST /teams"].Props[facts.PropSource], facts.RouteSourceHTTP4s)
	}
}

// TestHTTP4sIgnoresNonRoutePatternMatches guards against reading any `case A -> B`
// as an endpoint: the arrow is also ordinary tuple syntax.
func TestHTTP4sIgnoresNonRoutePatternMatches(t *testing.T) {
	src := `package p

object S {
  val m = Map("a" -> 1, "b" -> 2)
  def pick(x: Any) = x match {
    case key -> value => value
  }
}
`
	if ff := extractRoutes(t, "src/S.scala", src); len(ff) != 0 {
		t.Errorf("emitted %d routes from ordinary tuple/pattern syntax: %v", len(ff), routeKeys(ff))
	}
}

// TestRoutesAreServerRole pins that every server-side route carries role=server, so
// the cross-repo linker matches them against client calls rather than each other.
func TestRoutesAreServerRole(t *testing.T) {
	src := `package p

import org.http4s.HttpRoutes

object Api {
  val routes = HttpRoutes.of[IO] { case GET -> Root / "ping" => Ok() }
}
`
	for _, f := range extractRoutes(t, "src/Api.scala", src) {
		if f.Kind != facts.KindRoute {
			continue
		}
		if f.Props[facts.PropRole] != facts.RoleServer {
			t.Errorf("%s: role = %v, want server", f.Name, f.Props[facts.PropRole])
		}
	}
}

// TestPekkoRouteExtractionIsDeterministic exists because it was not. Verb selection
// used to iterate a Go map, whose order is randomized per run, so a route's method
// varied between extractions of identical source — and the resulting test failed
// only on re-run. enola's whole promise is that the same tree yields the same graph,
// so the property is asserted directly rather than inferred from a passing run.
func TestPekkoRouteExtractionIsDeterministic(t *testing.T) {
	src := `package p

import org.apache.pekko.http.scaladsl.server.Directives._

object Server {
  val routes =
    pathPrefix("api") {
      path("users") { get { complete(OK) } } ~
      (path("teams") & post) { complete(OK) } ~
      path("both") { get { complete(OK) } ~ delete { complete(OK) } }
    }
}
`
	first := routeSignature(extractRoutes(t, "src/Server.scala", src))
	for i := 0; i < 50; i++ {
		if got := routeSignature(extractRoutes(t, "src/Server.scala", src)); got != first {
			t.Fatalf("route extraction is not deterministic:\n run 1: %s\n run %d: %s", first, i+2, got)
		}
	}
	// A path block naming two verbs serves both, so both are emitted.
	got := routeFacts(extractRoutes(t, "src/Server.scala", src))
	for _, want := range []string{"GET /api/both", "DELETE /api/both"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; have %v", want, routeKeys(extractRoutes(t, "src/Server.scala", src)))
		}
	}
}

// routeSignature renders routes in emission order, so a difference in order or in
// any single method shows up as a different string.
func routeSignature(ff []facts.Fact) string {
	var b strings.Builder
	for _, f := range ff {
		if f.Kind != facts.KindRoute {
			continue
		}
		m, _ := f.Props["method"].(string)
		b.WriteString(m + " " + f.Name + ";")
	}
	return b.String()
}

// An http4s route's handler is the body of its case clause, which has no symbol of
// its own: it is given one, "<dir>.<Type>.handler:<VERB>:<line>", that carries the
// body's calls, and the route is handled by it.
func TestHTTP4sCaseBodyIsTheHandler(t *testing.T) {
	src := `package demo

import org.http4s.HttpRoutes

final class Routes(users: UsersDB) {
  val routes = HttpRoutes.of[IO] {
    case GET -> Root / "users" / UUIDVar(id) =>
      users.get(id).flatMap(u => Ok(u.name))
    case POST -> Root / "users" / name =>
      Audit.record(name)
  }
}
`
	ff, _ := extractFileFacts([]byte(src), "src/Routes.scala", map[string]string{})
	for key, handler := range map[string]string{
		"GET /users/:id":    "src.Routes.handler:GET:7",
		"POST /users/:name": "src.Routes.handler:POST:9",
	} {
		route, ok := routeFacts(ff)[key]
		if !ok {
			t.Errorf("missing route %s; have %v", key, routeKeys(ff))
			continue
		}
		if !hasRelation(route, facts.RelHandledBy, handler) {
			t.Errorf("%s relations = %v, want handled_by %s", key, route.Relations, handler)
		}
		sym := findFact(t, ff, handler)
		if !sym.PropBool("framework_registered") {
			t.Errorf("%s is not framework_registered", handler)
		}
		calls := 0
		for _, r := range sym.Relations {
			if r.Kind == facts.RelCalls || r.Kind == facts.RelInstantiates {
				calls++
			}
		}
		if calls == 0 {
			t.Errorf("%s relations = %v, want the case body's calls", handler, sym.Relations)
		}
	}
}
