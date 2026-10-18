package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The layout every Express tutorial teaches and serverroutes.go could not resolve:
// the router in one file, the mount in another. Before the repo-wide pass this
// project contributed zero routes.
func TestRouterMount_ESMDefaultExportComposesAcrossFiles(t *testing.T) {
	got := serverRoutes(extractAll(t, map[string]string{
		"src/server.ts": `
import express from "express";
import ordersRouter from "./api/orders.js";

const app = express();
app.use("/api", ordersRouter);
app.get("/healthcheck", handler);
`,
		"src/api/orders.ts": `
import express from "express";

const router = express.Router();
router.get("/orders", listOrders);
router.post("/orders/:id/refund", refundOrder);

export default router;
`,
	}, false))

	for path, want := range map[string]string{
		"/api/orders":            "GET",
		"/api/orders/:id/refund": "POST",
		"/healthcheck":           "GET",
	} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
	// The fragment must never survive: it is the wrong-fact case the whole pass is
	// arranged to avoid, and a wrong path can false-match another repo's route.
	if _, found := got["/orders"]; found {
		t.Errorf("the unmounted fragment was emitted alongside the composed path: %+v", got)
	}
}

// CommonJS is not a legacy shape here: the one real Express server in the corpus is
// written this way, which is why serverroutes.go binds `require('express')()` too.
func TestRouterMount_CommonJSRequireComposesAcrossFiles(t *testing.T) {
	got := serverRoutes(extractAll(t, map[string]string{
		"server/index.js": `
const app = require('express')();
const webhookRoutes = require('./routes/webhooks');

app.use('/webhooks', webhookRoutes);
`,
		"server/routes/webhooks.js": `
const express = require('express');
const router = express.Router();

router.post('/login', handler);
router.get('/login', handler);

module.exports = router;
`,
	}, false))

	if got["/webhooks/login"] == "" {
		t.Errorf("a CommonJS sub-router did not compose: %+v", got)
	}
	if _, found := got["/login"]; found {
		t.Errorf("fragment emitted: %+v", got)
	}
}

// `app.use('/api', routes())` — the router is built and returned by a function
// rather than exported as a value. Resolving it needs the callee's return, which is
// why collectRouterFactories exists.
func TestRouterMount_FactoryCallResolvesToTheRouterItReturns(t *testing.T) {
	got := serverRoutes(extractAll(t, map[string]string{
		"src/server.ts": `
import express from "express";
import { routes } from "./api/routes.js";

const app = express();
app.use("/api", routes());
`,
		"src/api/routes.ts": `
import express, { Router } from "express";

export function routes(): Router {
  const router = express.Router();
  router.get("/orders", listOrders);
  return router;
}
`,
	}, false))

	if got["/api/orders"] != "GET" {
		t.Errorf("a factory-returned router did not compose: %+v", got)
	}
}

// Two levels, three files. The middle router is mounted by the app and mounts a
// child of its own, so its prefix has to reach the grandchild — a fixpoint, not one
// hop.
func TestRouterMount_NestedRoutersComposeTransitively(t *testing.T) {
	got := serverRoutes(extractAll(t, map[string]string{
		"src/server.ts": `
import express from "express";
import apiRouter from "./api/index.js";

const app = express();
app.use("/api", apiRouter);
`,
		"src/api/index.ts": `
import express from "express";
import ordersRouter from "./orders.js";

const apiRouter = express.Router();
apiRouter.use("/orders", ordersRouter);
apiRouter.get("/status", status);

export default apiRouter;
`,
		"src/api/orders.ts": `
import express from "express";

const router = express.Router();
router.get("/:id", getOrder);

export default router;
`,
	}, false))

	for path, want := range map[string]string{
		"/api/status":     "GET",
		"/api/orders/:id": "GET",
	} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
	// "/orders/:id" is what a single hop, or the pre-fix same-file mount onto an
	// unmounted parent, would have produced.
	if _, found := got["/orders/:id"]; found {
		t.Errorf("a partial prefix escaped: %+v", got)
	}
}

// The "api router" layout: an intermediate router mounted at /api in the entry file,
// with per-domain routers from other files hung off it. The intermediate router's
// prefix is known in its own file, so the repo-wide pass must start from it; it used to
// accept only an app or an unmounted router as a parent and dropped the edge, so the
// whole subtree contributed nothing. Both statement orders, since Express resolves the
// mounts at request time.
func TestRouterMount_ImportedChildUnderLocallyMountedParent(t *testing.T) {
	routes := `
const express = require('express');
const router = express.Router();

router.get('/list', listOrders);
router.post('/create', createOrder);

module.exports = router;
`
	for name, mounts := range map[string]string{
		"parent mounted first": "app.use('/api', apiRouter);\napiRouter.use('/orders', ordersRoutes);",
		"child mounted first":  "apiRouter.use('/orders', ordersRoutes);\napp.use('/api', apiRouter);",
	} {
		t.Run(name, func(t *testing.T) {
			ff := extractAll(t, map[string]string{
				"server/index.js": `
const express = require('express');
const app = express();
const apiRouter = express.Router();
const ordersRoutes = require('./routes/orders');

apiRouter.get('/status', status);
` + mounts + `
`,
				"server/routes/orders.js": routes,
			}, false)
			got := serverRoutes(ff)

			for path, want := range map[string]string{
				"/api/orders/list":   "GET",
				"/api/orders/create": "POST",
				"/api/status":        "GET",
			} {
				if got[path] != want {
					t.Errorf("%s: want %s, got %+v", path, want, got)
				}
			}
			for _, fragment := range []string{"/list", "/create", "/orders/list", "/orders/create", "/status"} {
				if _, found := got[fragment]; found {
					t.Errorf("fragment %s emitted: %+v", fragment, got)
				}
			}
			// Seeding the parent's own prefix must not make the pass claim the
			// parent's own routes, which serverroutes.go already emits.
			counts := map[string]int{}
			for _, f := range ff {
				if f.Kind == facts.KindRoute {
					counts[f.Name]++
				}
			}
			for _, path := range []string{"/api/orders/list", "/api/orders/create", "/api/status"} {
				if counts[path] != 1 {
					t.Errorf("%s emitted %d times, want 1", path, counts[path])
				}
			}
		})
	}
}

// A router mounted at two prefixes really does serve both, so it emits once per
// mount — the same rule the Go and Axum passes follow.
func TestRouterMount_MountedTwiceEmitsBothPaths(t *testing.T) {
	got := serverRoutes(extractAll(t, map[string]string{
		"src/server.ts": `
import express from "express";
import router from "./api/orders.js";

const app = express();
app.use("/v1", router);
app.use("/v2", router);
`,
		"src/api/orders.ts": `
import express from "express";
const router = express.Router();
router.get("/orders", listOrders);
export default router;
`,
	}, false))

	for _, path := range []string{"/v1/orders", "/v2/orders"} {
		if got[path] != "GET" {
			t.Errorf("%s missing: %+v", path, got)
		}
	}
}

// Named exports, including a rename in either clause. `export { router as api }`
// and `import { api }` write the two names in opposite orders, and getting that
// backwards resolves to nothing at best and to the wrong router at worst.
func TestRouterMount_RenamedNamedExportResolves(t *testing.T) {
	got := serverRoutes(extractAll(t, map[string]string{
		"src/server.ts": `
import express from "express";
import { api as ordersRouter } from "./api/orders.js";

const app = express();
app.use("/api", ordersRouter);
`,
		"src/api/orders.ts": `
import express from "express";
const router = express.Router();
router.get("/orders", listOrders);
export { router as api };
`,
	}, false))

	if got["/api/orders"] != "GET" {
		t.Errorf("a renamed named export did not resolve: %+v", got)
	}
}

// Everything the pass cannot resolve keeps the pre-existing behaviour: silence. A
// non-literal prefix is the important one — `app.use(base, router)` composes a path
// only if you are willing to guess what `base` holds.
func TestRouterMount_UnresolvableMountsStaySilent(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"dynamic prefix": {
			"src/server.ts": `
import express from "express";
import router from "./api/orders.js";
const base = process.env.BASE_PATH;
const app = express();
app.use(base, router);
`,
			"src/api/orders.ts": `
import express from "express";
const router = express.Router();
router.get("/orders", listOrders);
export default router;
`,
		},
		"mounted by nobody": {
			"src/api/orders.ts": `
import express from "express";
const router = express.Router();
router.get("/orders", listOrders);
export default router;
`,
		},
		"external module": {
			"src/server.ts": `
import express from "express";
import router from "@vendor/orders-router";
const app = express();
app.use("/api", router);
`,
		},
	} {
		if got := serverRoutes(extractAll(t, files, false)); len(got) != 0 {
			t.Errorf("%s: want no routes, got %+v", name, got)
		}
	}
}

// A mount cycle is not a shape to follow forever. Neither router is reachable from
// an application, so nothing is emitted — and the pass terminates.
func TestRouterMount_CycleTerminatesAndEmitsNothing(t *testing.T) {
	got := serverRoutes(extractAll(t, map[string]string{
		"src/a.ts": `
import express from "express";
import b from "./b.js";
const a = express.Router();
a.get("/a", handler);
a.use("/to-b", b);
export default a;
`,
		"src/b.ts": `
import express from "express";
import a from "./a.js";
const b = express.Router();
b.get("/b", handler);
b.use("/to-a", a);
export default b;
`,
	}, false))

	if len(got) != 0 {
		t.Errorf("routers reachable only from each other must emit nothing, got %+v", got)
	}
}

// A router mounted in its own file is already emitted by serverroutes.go. If the
// repo-wide pass claimed it too, every such route would appear twice.
func TestRouterMount_LocallyMountedRouterIsNotEmittedTwice(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"server/index.js": `
const express = require('express');
const app = express();
const admin = express.Router();

admin.get('/users', listUsers);
app.use('/admin', admin);
`,
	}, false)

	n := 0
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Name == "/admin/users" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("/admin/users emitted %d times, want 1", n)
	}
}

// The composed marker records that a path was assembled from two files rather than
// read off one line, so a reader of the graph can tell the two apart. A route whose
// mount was in its own file carries no such claim.
func TestRouterMount_ComposedRoutesAreMarked(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/server.ts": `
import express from "express";
import router from "./api/orders.js";
const app = express();
app.use("/api", router);
app.get("/healthcheck", handler);
`,
		"src/api/orders.ts": `
import express from "express";
const router = express.Router();
router.get("/orders", listOrders);
export default router;
`,
	}, false)

	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/api/orders", true},
		{"/healthcheck", false},
	} {
		f, ok := findFact(ff, tc.path)
		if !ok {
			t.Fatalf("%s not extracted", tc.path)
		}
		if got := f.Props["mount_composed"] == true; got != tc.want {
			t.Errorf("%s: mount_composed = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Mounting onto a sub-router the file does not itself mount used to compose the
// child against a prefix of "", emitting the fragment "/orders/:id" for a route
// that really serves "/api/orders/:id". Silence is the floor; the repo-wide pass
// then supplies the rest.
func TestServerRoutes_MountOntoUnmountedParentEmitsNothing(t *testing.T) {
	src := `
const express = require('express');
const apiRouter = express.Router();
const orders = express.Router();

orders.get('/:id', getOrder);
apiRouter.use('/orders', orders);

module.exports = apiRouter;
`
	if got := serverRoutes(extractTS(t, src, "src/api/index.js")); len(got) != 0 {
		t.Errorf("a mount onto an unmounted parent must emit nothing, got %+v", got)
	}
}
