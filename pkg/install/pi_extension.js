// enola for Pi. Written by `enola install`; removed by `enola uninstall`.
//
// Pi has no MCP client, by design, so every enola tool would otherwise be a name in an
// instruction file that nothing serves. This extension is the client: it starts the
// enola MCP server over stdio, lists its tools and registers each one with Pi under an
// `enola_` prefix. The schemas are passed through untouched (Pi validates against plain
// JSON Schema), so a tool added to the server appears here with no change to this file.
//
// With `--hooks` it also runs the two session hooks Claude Code and Codex get, through
// Pi's events instead of a hook config:
//
//   - session_start (startup, resume) runs `enola hook session-start`, which pins the
//     baseline in the background and returns at once.
//   - agent_end runs `enola hook stop`. When it has a report, the report is sent back
//     as a message that starts one more turn, which is what a Stop hook's
//     additionalContext does in Claude Code. The agent_end that closes THAT turn is
//     flagged stop_hook_active, so the hook's own loop breaker applies unchanged.
//
// It imports nothing outside Node, so it loads as the single file it is written as, and
// every failure is silent to the model: a server that does not start means no enola
// tools, not a broken session.
//
// JavaScript, not TypeScript, although Pi loads both. The file sits inside the
// repository, and enola's own TypeScript extractor detects a repository by any `.ts`
// file: a `.ts` extension turned a Go repository into a TypeScript one, changed its
// extractor set, and left every baseline pinned before the install ungradable.

import { spawn } from "node:child_process"

// Substituted by `enola install`.
const ENOLA = __ENOLA_COMMAND__
const HOOKS = __ENOLA_HOOKS__

const PREFIX = "enola_"
const START_TIMEOUT_MS = 30_000
// Matches hookTimeoutSeconds in the Go installer.
const HOOK_TIMEOUT_MS = 60_000

// A minimal MCP client over newline-delimited JSON-RPC on stdio: initialize,
// tools/list and tools/call are all this needs, and a dependency would stop the file
// loading on its own.
class Server {
  next = 1
  pending = new Map()
  stderr = []
  closed = false

  constructor(cwd) {
    this.proc = spawn(ENOLA, [], { cwd, stdio: ["pipe", "pipe", "pipe"] })
    let buf = ""
    this.proc.stdout.setEncoding("utf8")
    this.proc.stdout.on("data", (chunk) => {
      buf += chunk
      let nl
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim()
        buf = buf.slice(nl + 1)
        if (line) this.dispatch(line)
      }
    })
    // Drained so a chatty server cannot fill the pipe and stall; the tail is kept for
    // the one message shown when the server fails to start.
    this.proc.stderr.setEncoding("utf8")
    this.proc.stderr.on("data", (chunk) => {
      this.stderr.push(chunk)
      if (this.stderr.length > 20) this.stderr.shift()
    })
    const fail = (why) => {
      this.closed = true
      for (const p of this.pending.values()) p.reject(new Error(why))
      this.pending.clear()
    }
    this.proc.on("error", (e) => fail(`enola could not be started: ${e.message}`))
    this.proc.on("exit", (code) => fail(`the enola server exited (code ${code})`))
  }

  dispatch(line) {
    let msg
    try {
      msg = JSON.parse(line)
    } catch {
      return
    }
    if (msg.id === undefined || msg.method !== undefined) return
    const p = this.pending.get(msg.id)
    if (!p) return
    this.pending.delete(msg.id)
    if (msg.error) p.reject(new Error(msg.error.message ?? "MCP error"))
    else p.resolve(msg.result)
  }

  send(msg) {
    if (!this.closed) this.proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", ...msg }) + "\n")
  }

  request(method, params, signal) {
    if (this.closed) return Promise.reject(new Error("the enola server is not running"))
    const id = this.next++
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      signal?.addEventListener("abort", () => {
        if (!this.pending.delete(id)) return
        this.send({ method: "notifications/cancelled", params: { requestId: id } })
        reject(new Error("cancelled"))
      })
      this.send({ id, method, params })
    })
  }

  async start() {
    const timer = new Promise((_, reject) =>
      setTimeout(() => reject(new Error("the enola server did not answer within 30s")), START_TIMEOUT_MS),
    )
    const handshake = async () => {
      await this.request("initialize", {
        protocolVersion: "2025-06-18",
        capabilities: {},
        clientInfo: { name: "enola-pi", version: "1" },
      })
      this.send({ method: "notifications/initialized" })
      const tools = []
      let cursor
      do {
        const page = await this.request("tools/list", cursor ? { cursor } : {})
        tools.push(...(page?.tools ?? []))
        cursor = page?.nextCursor
      } while (cursor)
      return tools
    }
    return Promise.race([handshake(), timer])
  }

  stop() {
    this.closed = true
    try {
      this.proc.stdin.end()
      this.proc.kill()
    } catch {}
  }
}

// runHook pipes a Claude-shaped payload into `enola hook <event>` and returns what it
// printed. It never rejects: a hook must not break the session it runs in.
function runHook(event, payload, wait) {
  return new Promise((resolve) => {
    let out = ""
    let child
    try {
      child = spawn(ENOLA, ["hook", event], { stdio: ["pipe", wait ? "pipe" : "ignore", "ignore"] })
    } catch {
      return resolve("")
    }
    child.on("error", () => resolve(""))
    child.stdin.on("error", () => {})
    child.stdin.end(JSON.stringify(payload))
    if (!wait) {
      child.unref()
      return resolve("")
    }
    const timer = setTimeout(() => {
      child.kill()
      resolve("")
    }, HOOK_TIMEOUT_MS)
    child.stdout.setEncoding("utf8")
    child.stdout.on("data", (c) => (out += c))
    child.on("exit", () => {
      clearTimeout(timer)
      resolve(out)
    })
  })
}

// toolSchema makes an MCP input schema acceptable to every provider Pi talks to. A tool
// with no arguments arrives as a bare `{"type":"object"}`, which is valid JSON Schema
// but is rejected by OpenAI-compatible endpoints (LM Studio among them) for lacking
// `properties`, and that rejection fails the whole request, not just the one tool.
function toolSchema(schema) {
  const out = { type: "object", ...(schema ?? {}) }
  if (!out.properties) out.properties = {}
  return out
}

function toContent(result) {
  const items = (result?.content ?? [])
    .map((c) => (c.type === "image" ? { type: "image", data: c.data, mimeType: c.mimeType } : c.type === "text" ? { type: "text", text: c.text } : null))
    .filter(Boolean)
  if (items.length === 0 && result?.structuredContent !== undefined) {
    items.push({ type: "text", text: JSON.stringify(result.structuredContent) })
  }
  return items.length ? items : [{ type: "text", text: "(no output)" }]
}

export default function (pi) {
  let server
  let starting
  const registered = new Set()
  let continuing = false

  const connect = (ctx) => {
    if (server && !server.closed) return starting
    const s = new Server(ctx.cwd)
    server = s
    starting = s
      .start()
      .then((tools) => {
        for (const t of tools) {
          const name = PREFIX + t.name
          // Registered once per runtime; a restarted server serves the same tools, and
          // execute() always reaches whichever server is current.
          if (registered.has(name)) continue
          registered.add(name)
          pi.registerTool({
            name,
            label: `enola ${t.name}`,
            description: t.description ?? t.name,
            parameters: toolSchema(t.inputSchema),
            async execute(_id, params, signal) {
              if (!server || server.closed) throw new Error("the enola server is not running; restart the Pi session")
              const res = await server.request("tools/call", { name: t.name, arguments: params ?? {} }, signal)
              const content = toContent(res)
              if (res?.isError) throw new Error(content.map((c) => c.text ?? "").join("\n") || "enola tool failed")
              return { content, details: undefined }
            },
          })
        }
      })
      .catch((e) => {
        s.stop()
        const tail = s.stderr.join("").trim().split("\n").slice(-3).join("\n")
        if (ctx.hasUI) ctx.ui.notify(`enola: ${e.message}${tail ? "\n" + tail : ""}`, "warning")
      })
    return starting
  }

  pi.on("session_start", async (event, ctx) => {
    if (HOOKS && (event.reason === "startup" || event.reason === "resume")) {
      runHook("session-start", { cwd: ctx.cwd, session_id: ctx.sessionManager?.getSessionId?.(), hook_event_name: "SessionStart", source: event.reason }, false)
    }
    await connect(ctx)
  })

  // Tells the model the tools' names as Pi registered them. The instruction files say
  // `explore`; the callable tool is `enola_explore`, and a weak model does not always
  // make that step on its own. Added only while the server is actually serving.
  pi.on("before_agent_start", (event) => {
    if (registered.size === 0 || !server || server.closed) return
    return {
      systemPrompt:
        event.systemPrompt +
        `\n\nenola serves this codebase's structure as a queryable index: modules, symbols, routes, ` +
        `storage, and how they depend on each other. Its tools are registered here with the ${PREFIX} ` +
        `prefix (for example ${PREFIX}explore, ${PREFIX}query_facts, ${PREFIX}impact_analysis). Before ` +
        `grep, find or reading files to work out how something is wired, call one of them. If enola ` +
        `reports no facts for this repository, call ${PREFIX}generate_snapshot once and continue.`,
    }
  })

  pi.on("agent_end", async (_event, ctx) => {
    if (!HOOKS) return
    const active = continuing
    continuing = false
    const out = await runHook("stop", { cwd: ctx.cwd, session_id: ctx.sessionManager?.getSessionId?.(), hook_event_name: "Stop", stop_hook_active: active }, true)
    let report = ""
    try {
      report = JSON.parse(out.trim().split("\n").pop() || "{}")?.hookSpecificOutput?.additionalContext ?? ""
    } catch {}
    if (!report) return
    continuing = true
    pi.sendMessage({ customType: "enola", content: report, display: true }, { triggerTurn: true })
  })

  pi.on("session_shutdown", () => {
    server?.stop()
    server = undefined
  })
}
