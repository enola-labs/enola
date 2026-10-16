package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// goRoutes extracts a module written from files and returns "<METHOD> <path>" ->
// the route's handler prop.
func goRoutes(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	repo := t.TempDir()
	files["go.mod"] = "module example.com/app\n\ngo 1.22\n"
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
		if f.Kind == facts.KindRoute {
			out[f.PropString("method")+" "+f.Name] = f.PropString("handler")
		}
	}
	return out
}

func wantGoRoutes(t *testing.T, got map[string]string, want map[string]string, absent ...string) {
	t.Helper()
	for key, handler := range want {
		have, ok := got[key]
		if !ok {
			t.Errorf("no route %q; have %v", key, got)
			continue
		}
		if have != handler {
			t.Errorf("%s handler = %q, want %q", key, have, handler)
		}
	}
	for _, key := range absent {
		if _, found := got[key]; found {
			t.Errorf("route %q must not be emitted; have %v", key, got)
		}
	}
}

// A path group's routes are served under the group, one per method, and handled
// by the last argument.
func TestExtractRoutes_PathGroupMatchPath(t *testing.T) {
	got := goRoutes(t, map[string]string{
		"routers/web.go": `package routers

import "github.com/go-chi/chi/v5"

func Routes() chi.Router {
	m := chi.NewRouter()
	m.Group("/{owner}/{repo}", func() {
		m.PathGroup("/compare/*", func(g *PathGroup) {
			g.MatchPath("GET", "/<basehead:*>.diff", mustNotBeEmpty, DownloadDiff)
			g.MatchPath("HEAD,GET", ` + "`/<image:*>/blobs/<digest>`" + `, GetBlob)
			g.MatchPath("POST", "/<*:*>", mustNotBeEmpty, bind(form{}), CreatePullRequest)
		})
	})
	return m
}
`,
	})
	wantGoRoutes(t, got, map[string]string{
		"GET /{owner}/{repo}/compare/{basehead}.diff":         "DownloadDiff",
		"GET /{owner}/{repo}/compare/{image}/blobs/{digest}":  "GetBlob",
		"HEAD /{owner}/{repo}/compare/{image}/blobs/{digest}": "GetBlob",
		"POST /{owner}/{repo}/compare/*":                      "CreatePullRequest",
	})
}

// A group or a mount whose path is a string constant, of this package or an imported
// one, or a local variable that holds one, is read as if the path were written there.
func TestExtractRoutes_PathHeldInAConstantOrVariable(t *testing.T) {
	got := goRoutes(t, map[string]string{
		"routers/init.go": `package routers

import (
	"github.com/go-chi/chi/v5"

	"example.com/app/routers/actions"
)

func NormalRoutes() chi.Router {
	r := chi.NewRouter()
	prefix := "/api/pipeline"
	r.Mount(prefix, actions.ArtifactsRoutes(prefix))
	prefix = actions.V4Base
	r.Mount(prefix, actions.V4Routes(prefix))
	prefix = compute()
	r.Mount(prefix, actions.UnknownRoutes(prefix))
	return r
}
`,
		"routers/actions/routes.go": `package actions

import "github.com/go-chi/chi/v5"

const artifactBase = "/_apis/artifacts"

const V4Base = "/twirp/ArtifactService"

func ArtifactsRoutes(prefix string) chi.Router {
	m := chi.NewRouter()
	m.Group(artifactBase, func() {
		m.Put("/{hash}/upload", upload)
	})
	m.Put(artifactBase, summary)
	return m
}

func V4Routes(prefix string) chi.Router {
	m := chi.NewRouter()
	m.Group("", func() {
		m.Post("CreateArtifact", create)
	})
	m.Group("/{index}", func() {
		m.Get(".diff", diff)
	})
	return m
}

func UnknownRoutes(prefix string) chi.Router {
	m := chi.NewRouter()
	m.Get("/unknown", unknown)
	return m
}
`,
	})
	wantGoRoutes(t, got, map[string]string{
		"PUT /api/pipeline/_apis/artifacts/{hash}/upload": "upload",
		"PUT /api/pipeline/_apis/artifacts":               "summary",
		"POST /twirp/ArtifactService/CreateArtifact":      "create",
		"GET /twirp/ArtifactService/{index}.diff":         "diff",
	}, "PUT /{hash}/upload", "POST CreateArtifact", "POST /CreateArtifact")
}

// A concatenated path is known when every part of it is: literals, string
// constants and local strings. A part computed at run time makes the whole path
// unknown, and the route is not published under the part that was readable.
func TestExtractRoutes_ConcatenatedPathNeedsEveryPart(t *testing.T) {
	got := goRoutes(t, map[string]string{
		"routers/paths.go": `package routers

const apiPrefix = "/api"

func prefix() string       { return "/p" }
func artifactPath() string { return "/a" }
`,
		"routers/chi.go": `package routers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func Routes(base string) chi.Router {
	m := chi.NewRouter()
	m.Get(apiPrefix+"/users", ListUsers)
	m.Get("/v1"+"/items", ListItems)
	path := artifactPath()
	m.Post(path+"*", Upload)
	m.Get(base+"/health", Health)
	return m
}

func ListUsers(w http.ResponseWriter, r *http.Request) {}
func ListItems(w http.ResponseWriter, r *http.Request) {}
func Upload(w http.ResponseWriter, r *http.Request)    {}
func Health(w http.ResponseWriter, r *http.Request)    {}
`,
		"routers/gin.go": `package routers

import "github.com/gin-gonic/gin"

func Gin(r *gin.Engine) {
	r.GET(apiPrefix+"/teams", ListTeams)
	r.GET(prefix()+"/hidden", Hidden)
}

func ListTeams(c *gin.Context) {}
func Hidden(c *gin.Context)    {}
`,
		"routers/std.go": `package routers

import "net/http"

func Std() {
	http.HandleFunc(apiPrefix+"/ping", Ping)
	http.HandleFunc(prefix()+"/pong", Pong)
}

func Ping(w http.ResponseWriter, r *http.Request) {}
func Pong(w http.ResponseWriter, r *http.Request) {}
`,
	})
	wantGoRoutes(t, got, map[string]string{
		"GET /api/users": "ListUsers",
		"GET /v1/items":  "ListItems",
		"GET /api/teams": "ListTeams",
		"ALL /api/ping":  "Ping",
	}, "POST *", "POST /*", "GET /health", "GET /hidden", "ALL /pong")
}

// A router built in a local variable and mounted under a path serves its routes
// under that path, wherever the Mount sits in the function, and a router mounted on
// a mounted router composes both paths.
func TestExtractRoutes_MountOfALocalRouter(t *testing.T) {
	got := goRoutes(t, map[string]string{
		"routers/web.go": `package routers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

const adminPath = "/admin"

func Routes() chi.Router {
	r := chi.NewRouter()
	api := chi.NewRouter()
	api.Get("/users", ListUsers)
	v2 := chi.NewRouter()
	v2.Post("/items", CreateItem)
	api.Mount("/v2", v2)
	r.Mount("/api", api)
	admin := chi.NewRouter()
	r.Mount(adminPath, admin)
	admin.Get("/stats", Stats)
	r.Get("/health", Health)
	return r
}

func Install() chi.Router {
	base := chi.NewRouter()
	r := chi.NewRouter()
	r.Get("/post-install", InstallDone)
	base.Mount("", r)
	root := chi.NewRouter()
	root.Get("/healthz", Health)
	base.Mount("/", root)
	return base
}

func InstallDone(w http.ResponseWriter, req *http.Request) {}
func ListUsers(w http.ResponseWriter, req *http.Request)  {}
func CreateItem(w http.ResponseWriter, req *http.Request) {}
func Stats(w http.ResponseWriter, req *http.Request)      {}
func Health(w http.ResponseWriter, req *http.Request)     {}
`,
	})
	wantGoRoutes(t, got, map[string]string{
		"GET /api/users":     "ListUsers",
		"POST /api/v2/items": "CreateItem",
		"GET /admin/stats":   "Stats",
		"GET /health":        "Health",
		"GET /post-install":  "InstallDone",
		"GET /healthz":       "Health",
	}, "GET /users", "POST /items", "GET /stats", "GET //post-install", "GET //healthz")
}
