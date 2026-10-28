package tsextractor

import (
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// eventSides lists, per event name, "role file" for every topic fact read as an
// in-process event.
func eventSides(ff []facts.Fact) map[string][]string {
	out := map[string][]string{}
	for _, f := range ff {
		if f.Kind == facts.KindStorage && f.PropString(facts.PropSource) == messagingSourceTSEventCall {
			out[f.Name] = append(out[f.Name], f.PropString(facts.PropMessagingRole)+" "+f.File)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func TestEvents_SubscriberAndConstantNamedProducers(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"packages/utils/src/events.ts": `export const OrderEvents = {
  /**
   * Emitted when an order is placed.
   * @eventPayload { id: string }
   */
  PLACED: "widget.placed",
}
export enum WebhookEvents {
  Received = "billing.callback_received",
}`,
		"packages/flows/src/complete.ts": `import { emitEventStep } from "@medusajs/framework/workflows-sdk"
import { OrderEvents } from "../../utils/src/events"
export function completeOrder() {
  emitEventStep({ eventName: OrderEvents.PLACED, data: {} })
}`,
		"packages/api/hooks/route.ts": `import { WebhookEvents } from "@medusajs/framework/utils"
export const POST = async (req) => {
  const eventBus = req.scope.resolve("event_bus")
  await eventBus.emit({ name: WebhookEvents.Received, data: {} })
}`,
		"packages/api/subscribers/webhook.ts": `import { SubscriberConfig } from "@medusajs/framework"
export default async function onWebhook({ event }) {}
export const config: SubscriberConfig = { event: WebhookEvents.Received }`,
		"packages/api/subscribers/search.ts": `import { SubscriberConfig } from "@medusajs/framework"
export default async function index({ event }) {}
export const config: SubscriberConfig = { event: [...new Set(indexes())] }`,
	}, false)
	got := eventSides(ff)
	want := map[string][]string{
		"widget.placed":             {"producer packages/flows/src/complete.ts"},
		"billing.callback_received": {"consumer packages/api/subscribers/webhook.ts", "producer packages/api/hooks/route.ts"},
	}
	for name, sides := range want {
		if len(got[name]) != len(sides) {
			t.Errorf("%s: %v, want %v", name, got[name], sides)
			continue
		}
		for i := range sides {
			if got[name][i] != sides[i] {
				t.Errorf("%s: %v, want %v", name, got[name], sides)
			}
		}
	}
	handler, _ := findFact(ff, "packages/api/subscribers.onWebhook")
	if !hasRelation(handler, facts.RelSubscribes, "billing.callback_received") {
		t.Errorf("subscriber handler does not subscribe: %v", handler.Relations)
	}
	producer, _ := findFact(ff, "packages/flows/src.completeOrder")
	if !hasRelation(producer, facts.RelPublishes, "widget.placed") {
		t.Errorf("workflow does not publish: %v", producer.Relations)
	}
	cfg, _ := findFact(ff, "packages/api/subscribers.config")
	unresolvedOnConfig := false
	for _, f := range ff {
		if f.Name == "packages/api/subscribers.config" && f.File == "packages/api/subscribers/search.ts" && f.PropString("unresolved_reason") == eventUnresolvedDynamic {
			unresolvedOnConfig = true
		}
	}
	if !unresolvedOnConfig {
		t.Errorf("a computed event list is not marked unresolved on its config: %v", cfg.Props)
	}
	for _, f := range ff {
		if f.Kind == facts.KindStorage && f.Name == "" {
			t.Error("an unresolved placeholder leaked into the facts")
		}
	}
}

func TestEvents_DirectusNodeAndBullMQ(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"api/src/socket.ts": `import emitter from "./emitter"
export function bind(client) {
  emitter.emitAction("websocket.connect", { client })
  emitter.onAction("websocket.error", () => {})
}`,
		"extensions/hooks/audit/index.ts": `import { defineHook } from "@directus/extensions-sdk"
export default defineHook(({ action, filter }) => {
  action("items.create", () => {})
})`,
		"src/jobs.ts": `import { EventEmitter } from "node:events"
import { Queue, Worker } from "bullmq"
const bus = new EventEmitter()
export function start() {
  bus.emit("job.started")
  bus.on("error", () => {})
  const q = new Queue("emails")
  new Worker("emails", async () => {})
}
document.addEventListener("click", () => {})`,
	}, false)
	got := eventSides(ff)
	for name, n := range map[string]int{"websocket.connect": 1, "websocket.error": 1, "items.create": 1, "src.bus:job.started": 1, "emails": 2} {
		if len(got[name]) != n {
			t.Errorf("%s: %v, want %d sides", name, got[name], n)
		}
	}
	for _, name := range []string{"error", "click"} {
		if _, ok := got[name]; ok {
			t.Errorf("%q is not an application event: %v", name, got[name])
		}
	}
}

// Two emitters' same-named events are different events.
func TestEvents_NodeEventsBelongToTheirEmitter(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/session.ts": `import { EventEmitter } from "events"
export class Session extends EventEmitter {
  ready() { this.emit("result") }
}`,
		"src/search.ts": `import { EventEmitter } from "events"
export class Search extends EventEmitter {
  run() { this.emit("result") }
  listen() { this.on("result", () => {}) }
}`,
	}, false)
	got := eventSides(ff)
	if len(got["src.Session:result"]) != 1 || len(got["src.Search:result"]) != 2 {
		t.Errorf("events not kept per emitter: %v", got)
	}
	if _, ok := got["result"]; ok {
		t.Error("a bare event name merges unrelated emitters")
	}
}

// An emit inside a local variable of a workflow is published by the workflow.
func TestEvents_LocalVariableClimbsToItsFunction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"flows/complete.ts": `import { emitEventStep } from "@medusajs/framework/workflows-sdk"
export const completeWorkflow = createWorkflow("complete", function (input) {
  const order = transform({ input }, () => {
    emitEventStep({ eventName: "widget.placed", data: {} })
  })
  return order
})`,
	}, false)
	wf, _ := findFact(ff, "flows.completeWorkflow")
	if !hasRelation(wf, facts.RelPublishes, "widget.placed") {
		t.Errorf("the workflow holding the emit does not publish: %v", wf.Relations)
	}
}

// A nested function is no symbol, and module-level code is held by its file.
func TestEvents_NestedFunctionAndModuleLevelCode(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"api/server.ts": `import emitter from "./emitter"
import { helper } from "./helper"
export async function startServer() {
  async function onShutdown() { emitter.emitAction("server.stop", {}) }
  helper(onShutdown)
}
router.post("/hook", async (req) => { emitter.emitAction("hook.received", {}) })`,
		"api/helper.ts": `export function helper(f) { return f }`,
	}, false)
	start, _ := findFact(ff, "api.startServer")
	if !hasRelation(start, facts.RelPublishes, "server.stop") {
		t.Errorf("the function holding the nested one does not publish: %v", start.Relations)
	}
	held := false
	for _, f := range ff {
		if f.Kind == facts.KindFileRef && f.File == "api/server.ts" && hasRelation(f, facts.RelPublishes, "hook.received") {
			held = true
		}
	}
	if !held {
		t.Error("module-level code's emit is not held by its file")
	}
}

// A Node event written as a constant resolves under its emitter; a conditional names
// both events; a variable is a name that cannot be read.
func TestEvents_NodeEventConstantsAndConditionals(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/types.ts": `export enum TxEvent { BEGIN = "begin", DONE = "done", FAILED = "failed", TIMEOUT = "timeout" }`,
		"src/orchestrator.ts": `import { EventEmitter } from "events"
import { TxEvent } from "./types"
export class Orchestrator extends EventEmitter {
  start() { this.emit(TxEvent.BEGIN); this.emit(TxEvent.TIMEOUT); this.emit("timeout") }
  finish(ok: boolean) { this.emit(ok ? TxEvent.DONE : TxEvent.FAILED) }
  relay(name: string) { this.emit(name) }
}`,
	}, false)
	got := eventSides(ff)
	// The literal "timeout" is a stream's own event and is dropped; the constant one is
	// the table's, and kept, once.
	for _, want := range []string{"src.Orchestrator:begin", "src.Orchestrator:done", "src.Orchestrator:failed", "src.Orchestrator:timeout"} {
		if len(got[want]) != 1 {
			t.Errorf("%s: %v (all %v)", want, got[want], got)
		}
	}
	relay, _ := findFact(ff, "src.Orchestrator.relay")
	if relay.PropString("unresolved_reason") != eventUnresolvedDynamic {
		t.Errorf("a variable event name is not marked: %v", relay.Props)
	}
}

// An emitter exported from one file is the same emitter where another file imports
// it, or builds one from its class. An imported object that is not an exported
// emitter is no event source.
func TestEvents_NodeEmittersAcrossFiles(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/migrations/index.ts": `import { EventEmitter } from "events"
export class Migrations extends EventEmitter {
  run() { this.emit("migrated") }
}`,
		"src/bus.ts": `import { EventEmitter } from "events"` + "\n" + `export const bus = new EventEmitter()`,
		"src/app.ts": `export const app = { on(_: string, f: () => void) {} }`,
		"src/up.ts": `import { Migrations } from "./migrations"
import { bus } from "./bus"
import { app } from "./app"
export function up() {
  const migrations = new Migrations()
  migrations.on("migrated", () => {})
  bus.emit("live-ready")
  app.on("mounted", () => {})
}`,
		"src/listen.ts": `import { bus } from "./bus"` + "\n" + `export function listen() { bus.on("live-ready", () => {}) }`,
	}, false)
	got := eventSides(ff)
	if len(got["src/migrations.Migrations:migrated"]) != 2 {
		t.Errorf("an instance of an imported emitter class: %v", got)
	}
	if len(got["src.bus:live-ready"]) != 2 {
		t.Errorf("an imported emitter instance: %v", got)
	}
	for name := range got {
		if name == "src.app:mounted" {
			t.Errorf("an imported object that is no emitter made an event: %v", got)
		}
	}
	up, _ := findFact(ff, "src.up")
	if !hasRelation(up, facts.RelSubscribes, "src/migrations.Migrations:migrated") || !hasRelation(up, facts.RelPublishes, "src.bus:live-ready") {
		t.Errorf("up's sides: %v", up.Relations)
	}
	for _, f := range ff {
		if f.PropString(propEmitterPending) != "" || f.PropString(propEventOwner) != "" {
			t.Errorf("a pending prop survived: %s %v", f.Name, f.Props)
		}
	}
}

// Angular's EventEmitter is a component output: emit takes a payload.
func TestEvents_AngularOutputIsNoNodeEvent(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/result.ts": `export enum Result { Saved = "saved" }`,
		"src/assign.component.ts": `import { EventEmitter, Output } from "@angular/core"
import { Result } from "./result"
export class AssignComponent {
  @Output() done = new EventEmitter<Result>()
  save() { this.done.emit(Result.Saved); this.done.emit("x") }
}`,
	}, false)
	if got := eventSides(ff); len(got) != 0 {
		t.Errorf("an Angular output read as Node events: %v", got)
	}
}
