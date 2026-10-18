package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// clientRoutes indexes emitted client-role routes by path -> method, the counterpart
// of serverRoutes in decoratorroutes_test.go.
func clientRoutes(ff []facts.Fact) map[string]string {
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props["role"] == "client" {
			out[f.Name] = f.Props["method"].(string)
		}
	}
	return out
}

// TestServerRoutes_ExpressApp covers the root case: routes registered on an
// application object are served at the path as written.
func TestServerRoutes_ExpressApp(t *testing.T) {
	src := `
const express = require('express');
const app = express();

app.get('/healthcheck', healthCheckController);
app.post('/go/:name', proxyLink());
app.get('*', redirects());
`
	got := serverRoutes(extractTS(t, src, "server/index.js"))
	if got["/healthcheck"] != "GET" {
		t.Errorf("app.get: %+v", got)
	}
	if got["/go/:name"] != "POST" {
		t.Errorf("app.post with a param: %+v", got)
	}
	// A bare catch-all is a SPA fallback, not an endpoint. Indexing it would let it
	// match any client path at all.
	if _, found := got["/*"]; found {
		t.Errorf("wildcard route must not be emitted: %+v", got)
	}
}

// TestServerRoutes_UnmountedRouterEmitsNothing is the correctness rule this pass is
// built around. A sub-router's paths are FRAGMENTS: `router.post('/login')` in a
// routes module that index.js mounts at '/webhooks' really serves
// '/webhooks/login'. Emitting '/login' would be a WRONG fact, and a wrong path can
// false-match another repo's route — worse than silence. This is the dominant
// layout for a routes/ directory, so the rule matters.
func TestServerRoutes_UnmountedRouterEmitsNothing(t *testing.T) {
	src := `
const express = require('express');
const router = express.Router();

router.post('/login', async (req, res) => {});
router.get('/login', async (req, res) => {});

module.exports = router;
`
	if got := serverRoutes(extractTS(t, src, "server/routes/webhooks.js")); len(got) != 0 {
		t.Errorf("a router with no visible mount must emit nothing, got %+v", got)
	}
}

// TestServerRoutes_SameFileMountComposes covers the mount this pass DOES resolve:
// declared and mounted in the same file, so the prefix is known without a repo-wide
// pass.
func TestServerRoutes_SameFileMountComposes(t *testing.T) {
	src := `
const express = require('express');
const app = express();
const router = express.Router();

router.get('/login', handler);
router.post('/logout', handler);

app.use('/webhooks', router);
app.get('/healthcheck', handler);
`
	got := serverRoutes(extractTS(t, src, "server/index.js"))
	for path, want := range map[string]string{
		"/webhooks/login":  "GET",
		"/webhooks/logout": "POST",
		"/healthcheck":     "GET",
	} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
	if _, found := got["/login"]; found {
		t.Errorf("unmounted path must not survive alongside the composed one: %+v", got)
	}
}

// TestServerRoutes_ChildMountedBeforeParentComposes is the same tree as the forward
// order, written the other way round: the child is mounted on apiRouter one statement
// before apiRouter is mounted on the app. Express resolves both at request time, so the
// route serves "/api/orders/list" either way. Resolving mounts in one pass in source
// order left the child unmounted and the route absent from the graph.
func TestServerRoutes_ChildMountedBeforeParentComposes(t *testing.T) {
	src := `
const express = require('express');
const app = express();
const apiRouter = express.Router();
const ordersRouter = express.Router();

ordersRouter.get('/list', handler);

apiRouter.use('/orders', ordersRouter);
app.use('/api', apiRouter);
`
	ff := extractTS(t, src, "server/index.js")
	got := serverRoutes(ff)
	if got["/api/orders/list"] != "GET" {
		t.Errorf("a child mounted before its parent did not compose: %+v", got)
	}
	for _, fragment := range []string{"/list", "/orders/list"} {
		if _, found := got[fragment]; found {
			t.Errorf("fragment %s emitted: %+v", fragment, got)
		}
	}
	// Both mounts are in this file, so the path is read off it rather than composed
	// across files: the same fact the forward order produces.
	n := 0
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Name == "/api/orders/list" {
			n++
			if f.Props["mount_composed"] == true {
				t.Errorf("a same-file mount was marked as composed across files: %+v", f)
			}
		}
	}
	if n != 1 {
		t.Errorf("/api/orders/list emitted %d times, want 1", n)
	}
}

// TestServerRoutes_DoNotStealClientCalls is the sharpest risk in this change.
// `axios.get('/x')` and `router.get('/x')` are the same text; v141 already emits the
// former as a client call. Only a receiver BOUND to an app/router in this file may
// become a server route, and an unknown receiver must keep its v141 behaviour
// exactly — reclassifying one would move existing facts.
func TestServerRoutes_DoNotStealClientCalls(t *testing.T) {
	src := `
import axios from "axios";
import { http } from "../lib/http";

export async function load() {
  await axios.get("/api/v2/slots/available");
  await http.post("/slots/reserve", body);
  await someUnknownThing.get("/unknown/receiver");
}
`
	ff := extractTS(t, src, "src/api/client.ts")
	client := clientRoutes(ff)
	for path, want := range map[string]string{
		"/api/v2/slots/available": "GET",
		"/slots/reserve":          "POST",
		"/unknown/receiver":       "GET",
	} {
		if client[path] != want {
			t.Errorf("%s must stay a CLIENT call: want %s, got %+v", path, want, client)
		}
	}
	if got := serverRoutes(ff); len(got) != 0 {
		t.Errorf("no server routes in a file with no app/router binding: %+v", got)
	}
}

// TestServerRoutes_NoDoubleEmission pins the other half of that boundary: a route
// registration must be emitted ONCE, as a server route, not also as an outbound
// client call by v141's receiver-agnostic pass.
func TestServerRoutes_NoDoubleEmission(t *testing.T) {
	src := `
const express = require('express');
const app = express();
app.get('/healthcheck', handler);
`
	ff := extractTS(t, src, "server/index.js")
	if got := serverRoutes(ff); got["/healthcheck"] != "GET" {
		t.Errorf("expected a server route: %+v", got)
	}
	if got := clientRoutes(ff); len(got) != 0 {
		t.Errorf("a route registration must not also be a client call: %+v", got)
	}
}

// TestServerRoutes_OtherFrameworks covers the rest of the family that shares the
// shape, so the framework prop is not silently wrong for them.
func TestServerRoutes_OtherFrameworks(t *testing.T) {
	for _, tc := range []struct{ decl, want string }{
		{"const app = new Hono();", "hono"},
		{"const app = Fastify();", "fastify"},
		{"const app = fastify();", "fastify"},
		{"const app = new Koa();", "koa"},
	} {
		src := tc.decl + "\napp.get('/health/check', handler);\n"
		ff := extractTS(t, src, "src/server.ts")
		var fw string
		for _, f := range ff {
			if f.Kind == facts.KindRoute && f.Props["role"] == "server" {
				fw = f.Props["framework"].(string)
			}
		}
		if fw != tc.want {
			t.Errorf("%s: framework want %q, got %q", tc.decl, tc.want, fw)
		}
	}
}

// TestServerRoutes_TestFileEmitsNothing mirrors the v141/v142 gates: an e2e suite
// that spins up its own app is not a production surface.
func TestServerRoutes_TestFileEmitsNothing(t *testing.T) {
	src := `
const app = express();
app.get('/fixture/route', handler);
`
	for _, f := range []string{"server/index.e2e-spec.ts", "server/app.spec.ts", "e2e/server.e2e.ts"} {
		if got := serverRoutes(extractTS(t, src, f)); len(got) != 0 {
			t.Errorf("%s: %+v", f, got)
		}
	}
}

func TestServerRoutes_QuoteStyleDoesNotDecideExtraction(t *testing.T) {
	// Group indices for the path captures were off by one: a double-quoted path
	// read the single-quote group, a single-quoted path read the backtick group,
	// and a path that only populated the double-quote group walked past the
	// match layout — a PANIC on `api.get("/health")`, the most ordinary
	// registration there is. All three styles must extract identically.
	src := []byte(`import Fastify from 'fastify';
const api = Fastify();
api.get("/health", handler);
api.post('/jobs', handler);
api.put(` + "`/queue`" + `, handler);
`)
	ff := extractServerRouteFacts(src, "src/index.ts")
	want := map[string]bool{"/health": false, "/jobs": false, "/queue": false}
	for _, f := range ff {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("route %q missing — quote style must not decide extraction", path)
		}
	}
}

func TestServerRoutes_DoubleQuotedMountPrefix(t *testing.T) {
	src := []byte(`const express = require('express');
const app = express();
const router = express.Router();
app.use("/api", router);
router.get("/health", handler);
`)
	ff := extractServerRouteFacts(src, "server/index.js")
	found := false
	for _, f := range ff {
		if f.Name == "/api/health" {
			found = true
		}
	}
	if !found {
		t.Errorf("double-quoted mount prefix did not compose; got %+v", ff)
	}
}
