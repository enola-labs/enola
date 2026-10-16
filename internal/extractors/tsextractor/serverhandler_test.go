package tsextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// routeHandlers returns "<METHOD> <path>" -> the handled_by target of each server
// route, "" when the route has none.
func routeHandlers(ff []facts.Fact) map[string]string {
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.PropString(facts.PropRole) != facts.RoleServer {
			continue
		}
		target := ""
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				target = r.Target
			}
		}
		out[f.PropString("method")+" "+f.Name] = target
	}
	return out
}

func wantHandlers(t *testing.T, got, want map[string]string) {
	t.Helper()
	for key, target := range want {
		have, ok := got[key]
		if !ok {
			t.Errorf("no route %q; have %v", key, got)
			continue
		}
		if have != target {
			t.Errorf("%s handled_by = %q, want %q", key, have, target)
		}
	}
}

// The handler is the last argument. What precedes it is middleware, and a wrapper
// around it is looked through.
func TestServerRouteHandler_LastArgumentThroughAWrapper(t *testing.T) {
	got := routeHandlers(extractAll(t, map[string]string{
		"src/server.ts": `
import express from "express";
import { authenticate, asyncMiddleware } from "./middlewares.js";

const app = express();

app.get("/plain", listThings);
app.post(
  "/upload",
  authenticate,
  validate(uploadSchema),
  asyncMiddleware(addThing)
);
app.get("/inline", authenticate, (req, res) => res.send("ok"));
app.put("/schema", authenticate, validate(uploadSchema));

function listThings(req, res) {}
async function addThing(req, res) {}
const uploadSchema = { title: "string" };
`,
		"src/middlewares.ts": `
export function authenticate(req, res, next) {}
export function asyncMiddleware(fn) { return fn }
`,
	}, false))
	wantHandlers(t, got, map[string]string{
		"GET /plain":   "src.listThings",
		"POST /upload": "src.addThing",
		"GET /inline":  "",
		"PUT /schema":  "",
	})
}

// A handler declared in another file is bound to its declaration there, on a router
// that is itself mounted from a third file.
func TestServerRouteHandler_ImportedHandlerOnAMountedRouter(t *testing.T) {
	got := routeHandlers(extractAll(t, map[string]string{
		"src/server.ts": `
import express from "express";
import { ordersRouter } from "./api/orders.js";

const app = express();
app.use("/api", ordersRouter);
`,
		"src/api/orders.ts": `
import express from "express";
import { listOrders } from "../handlers/index.js";

const ordersRouter = express.Router();
ordersRouter.get("/orders", listOrders);
ordersRouter.get("/missing", notDeclaredAnywhere);

export { ordersRouter };
`,
		"src/handlers/index.ts": `
export * from "./orders.js";
`,
		"src/handlers/orders.ts": `
export async function listOrders(req, res) {}
`,
	}, false))
	wantHandlers(t, got, map[string]string{
		"GET /api/orders":  "src/handlers.listOrders",
		"GET /api/missing": "",
	})
}

// A router that reaches its mount through a barrel composes like one imported
// directly, whichever re-export form the barrel uses.
func TestRouterMount_RouterImportedThroughABarrel(t *testing.T) {
	got := serverRoutes(extractAll(t, map[string]string{
		"server.ts": `
import express from "express";
import { apiRouter, feedsRouter } from "./controllers/index.js";

const app = express();
app.use("/api", apiRouter);
app.use("/", feedsRouter);
`,
		"controllers/index.ts": `
export * from "./api/index.js";
export { feedsRouter } from "./feeds.js";
`,
		"controllers/api/index.ts": `
import express from "express";
const apiRouter = express.Router();
apiRouter.get("/jobs", listJobs);
export { apiRouter };
`,
		"controllers/feeds.ts": `
import express from "express";
const feedsRouter = express.Router();
feedsRouter.get("/feeds/videos", videoFeeds);
export { feedsRouter };
`,
	}, false))
	for path, want := range map[string]string{"/api/jobs": "GET", "/feeds/videos": "GET"} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
}

// A mount prefix that appends a constant resolves when the constant is a string
// literal, in this file or in the one it is imported from, and not otherwise.
func TestRouterMount_PrefixWithAStringConstant(t *testing.T) {
	got := serverRoutes(extractAll(t, map[string]string{
		"server.ts": `
import express from "express";
import { API_VERSION, computed } from "./constants.js";
import { apiRouter } from "./api.js";
import { otherRouter } from "./other.js";

const app = express();
app.use("/api/" + API_VERSION, apiRouter);
app.use("/x/" + computed, otherRouter);
`,
		"constants.ts": `
export const API_VERSION = 'v1'
export const computed = process.env.PREFIX
`,
		"api.ts": `
import express from "express";
const apiRouter = express.Router();
apiRouter.get("/jobs", listJobs);
export { apiRouter };
`,
		"other.ts": `
import express from "express";
const otherRouter = express.Router();
otherRouter.get("/things", listThings);
export { otherRouter };
`,
	}, false))
	if got["/api/v1/jobs"] != "GET" {
		t.Errorf("the constant prefix did not compose: %+v", got)
	}
	for path := range got {
		if path == "/things" || path == "/x/things" || path == "/x/computed/things" {
			t.Errorf("a prefix that is not a string literal produced %s", path)
		}
	}
}

func TestLastCallArgument(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{`r.get('/a', h)`, "h"},
		{`r.get('/a', mw, wrap(h))`, "wrap(h)"},
		{`r.get('/a', mw({ a: [1, 2] }), "x,)", h) // tail`, "h"},
		{"r.get('/a', /* c, */ mw,\n  // d, e\n  h\n)", "h"},
		{`r.get('/a')`, ""},
		{`r.get('/a', h`, ""},
	} {
		// The scan starts where the route regexp stops: after the path literal.
		from := len(`r.get('/a'`)
		if got := lastCallArgument([]byte(tc.src), from); got != tc.want {
			t.Errorf("%s: last argument = %q, want %q", tc.src, got, tc.want)
		}
	}
}

// A call on a namespace import names the module's export, and it is a call of the
// function that makes it, not only a reference of the file.
func TestNamespaceImportCall_NamesTheExport(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/routes/load.ts": `import * as api from '../lib/api';
import * as path from 'node:path';

export async function load(token: string) {
	const tags = await api.get('tags', token);
	return path.join('a', tags);
}
`,
		"src/lib/api.ts": `export function get(p: string, token?: string) { return p }
`,
	}, false)
	var calls []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == "src/routes.load" {
			for _, r := range f.Relations {
				if r.Kind == facts.RelCalls {
					calls = append(calls, r.Target)
				}
			}
		}
	}
	if !slices.Contains(calls, "src/lib.get") {
		t.Errorf("load calls %v, want src/lib.get", calls)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "node:path") || strings.HasSuffix(c, ".join") {
			t.Errorf("load calls %v: a package namespace names no fact here", calls)
		}
	}
}

// A component's script resolves a call on a namespace import as an ordinary file
// does: a function of a .svelte or .vue component that calls api.get calls the
// module's export.
func TestNamespaceImportCall_InComponentScripts(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/routes/editor/Editor.svelte": `<script lang="ts">
	import * as api from '../../lib/api';

	async function save(token: string) {
		await api.get('articles', token);
	}
</script>

<button on:click={() => save('t')}>Save</button>
`,
		"src/components/Login.vue": `<script setup lang="ts">
import * as api from '../lib/api';

async function submit(token: string) {
	await api.get('users/login', token);
}
</script>

<template><button @click="submit('t')">Sign in</button></template>
`,
		"src/lib/api.ts": `export function get(p: string, token?: string) { return p }
`,
	}, false)
	callers := map[string]bool{}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls && r.Target == "src/lib.get" {
				callers[f.Name] = true
			}
		}
	}
	for _, want := range []string{"src/routes/editor.save", "src/components.submit"} {
		if !callers[want] {
			t.Errorf("%s does not call src/lib.get; callers %v", want, callers)
		}
	}
}

// A method called on a local built with `new`, or on a parameter annotated with a
// class, names that class's method. A global type names nothing.
func TestLocalTypes_NewAndAnnotatedParameters(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/upload.ts": `import { LocalVideoCreator } from './lib/creator';
import { VideoService } from './lib/service';

export async function addVideo(svc: VideoService, res: Response) {
	const creator = new LocalVideoCreator({ path: 'x' });
	const { video } = await creator.create();
	await svc.publish(video);
	return res.json(video);
}
`,
		"src/lib/creator.ts": `export class LocalVideoCreator {
	constructor(private opts: object) {}
	async create() { return { video: 1 } }
}
`,
		"src/lib/service.ts": `export class VideoService {
	async publish(v: number) {}
}
`,
	}, false)
	var calls []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == "src.addVideo" {
			for _, r := range f.Relations {
				if r.Kind == facts.RelCalls {
					calls = append(calls, r.Target)
				}
			}
		}
	}
	for _, want := range []string{"src/lib.LocalVideoCreator.create", "src/lib.VideoService.publish"} {
		if !slices.Contains(calls, want) {
			t.Errorf("addVideo calls %v, want %q", calls, want)
		}
	}
	for _, c := range calls {
		if strings.Contains(c, "Response") {
			t.Errorf("addVideo calls %v: the global Response names no fact", calls)
		}
	}
}
