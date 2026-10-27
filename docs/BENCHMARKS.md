# Benchmarks

This page carries three kinds of evidence: a field measurement from real coding-agent
sessions, an A/B of the same agent with and without enola on code it has never seen, and
a repeatable fixed-corpus benchmark of enola's structural verdicts.

The fixed corpus was measured on 2026-10-25, at extractor version v465, on 100 public
open-source repositories, with one binary, by scripts you can re-run. It carries the
latest sweep rather than a released version's, so it moves when the extractors do.
Where a number is unflattering it is still here.

## Real-session usage

Between February and October 2026, the logs written by Claude Code and OpenCode
recorded 4,375 enola calls across 238 sessions of ordinary development. Nothing was
staged or re-run.

Claude Code used the latest Claude Opus version available at the time throughout.
OpenCode ran local 27B-class Qwen models throughout: Qwen 3.8 27B at the end of the
measurement period, Qwen 3.6 27B before it, and Qwen Coder earlier.

| | Claude Code | OpenCode | Total |
|---|---:|---:|---:|
| Sessions | 32 | 206 | **238** |
| enola calls | 774 | 3,601 | **4,375** |
| Tokens enola put into context | 389k | 3.7M | **4.1M** |
| Opening only the files named in each answer | 5.4× as much | 7.7× as much | — |
| Reading the full source represented by each session | 450× as much | 623× as much | — |
| Snapshot sessions whose source exceeded median active context | 27 / 27 | 178 / 183 | **205 / 210** |

The median call was about 220 to 250 tokens. Most graph lookups completed in tens of
milliseconds; generating a snapshot took a median of 1.3 to 3.1 seconds. The full-source
comparison applies to questions that need repository-wide structure. The named-file
comparison is the narrower floor: it assumes the agent already knew which files to open,
and prices only those files.

The payload and latency come from the agents' own logs. No harness records tokens per
tool call, so tokens are estimated as characters sent and returned divided by four. The
two source-reading columns use repository source sizes and each harness's observed
behaviour when reading files. They describe this workload; another developer's repositories,
models and working style will produce different numbers under the same accounting.

The [full field report](https://enola.tech/blog/what-enola-saves-in-real-agent-sessions)
breaks the result down by tool and by one- versus multi-repository sessions.

## Agent A/B on code the model has not seen

The field measurement records work that happened once. This one asks the same question
twice, through Claude Code headless (`claude -p`, Opus 5.5), on 2026-10-22 and
2026-10-23:

- **bare**: no MCP servers, no enola instructions, only the shell and file reading.
- **enola**: enola over MCP, installed with `enola install --hooks` the way a user
  installs it, and the prompt line "Use enola to answer this." Nothing else differs.

Each graded question ran five trials per arm. The final runs used extractor v441, the
build released as v0.4.34.

**The code.** Five repositories built for one real product: a Go backend, a Next.js web
frontend, an iOS app, an Android app, and a Go authentication library all four use. It is
the kind of code these tools are used on most: built for a product someone works on every
day, and never in a model's training data. That is what matters. On the public
repositories in the corpus below, Opus 5.5 already knows the code, and a question about
gitea measured its memory of gitea more than its investigation. The harness is public: see
[Reproducing](#reproducing).

**The keys.** Every graded question has an answer key built from the code before any
session runs, never from enola: a type-checking Go program, `go list`, import resolvers
for TypeScript and Kotlin, and reading. Where an arm disagreed with a key, the code was
read again. Every session is priced as if its prompt cache started empty, so run order
cannot make an arm cheaper. A session offered a tool or an instruction its arm should not
have is recorded and not scored.

### Where enola changed the answer

Questions that span the backend and its clients, asked the way someone responsible for
the whole system asks them. Exact final answers out of five trials per arm; cost, time
and steps are medians per session.

| Question | Exact, bare | Exact, enola | Cold $, bare / enola | Time, bare / enola | Steps, bare / enola |
|---|---|---|---|---|---|
| Community endpoints no client calls | 0 / 5 | 5 / 5 | 1.03 / 0.71 | 137 s / 58 s | 22 / 12 |
| Pro shop endpoints no client calls | 2 / 5 | 4 / 5 | 1.00 / 0.71 | 134 s / 63 s | 20 / 11 |
| Pro shop endpoints no client ever sends | 2 / 5 | 4 / 5 | 1.93 / 0.93 | 265 s / 102 s | 49 / 16 |
| What a merged feature did to the structure | 0 / 5 | 5 / 5 | 0.77 / 0.90 | 139 s / 161 s | 20 / 26 |
| **Total** | **4 / 20** | **18 / 20** | | | |

The scores are the agent's final answers, not enola's output. enola's analysis was
correct in all 20 enola trials, and both misses happened after it reached the agent. In
one, the agent wrote `PUT` for an endpoint enola reported as `DELETE`. In the other, it
decided that three dead inventory endpoints enola had reported were outside the
question's scope.

- **Dead endpoints across clients.** About 105 community and 121 pro shop routes, each
  matched by path and method against the web app and both mobile apps. Many calls have
  no literal to search for: a URL built in a conditional or from a base field
  (`` `${this.baseUrl}/${id}` ``), called with a query string, split over two lines, or
  sent through a client instance another file exports. The bare arm found every dead
  community endpoint and named four to six live ones dead in every trial, mostly the same
  ones. Its trials agreed with each other at 0.95 and were wrong in all five. Agreement
  is not correctness.
- **Never sent.** The harder form counts a call only if code a page or component reaches
  sends it. The bare arm used the right method, following each client method through the
  facade, stores and hooks to a page by hand, and one of its five sessions hit the 60-turn
  limit before answering. The enola arm read the `unreached_by_clients` finding and
  checked it in a few files.
- **A merged feature.** The backend and frontend at the commits before and after a
  feature merge of about 16,000 lines. The key holds 80 items: endpoints added, added
  endpoints the frontend does not call, new dependencies between backend packages, and
  frontend folders that joined an import cycle. The enola arm snapshotted both commits,
  read `diff_snapshot`, and was exact every time. The bare arm was one or two items off
  every time, each time naming an endpoint uncalled that the frontend calls, exactly the
  kind of mistake someone might act on. The enola arm's sessions ran 13 cents and 22
  seconds longer, and were the only exact answers.

### Where both arms were right

Where one search, one toolchain command or one script the agent writes can answer the
question, Opus with a shell is already right, so these questions compare cost and time.
enola was cheaper and faster on the first two. On package metrics it returned the right
numbers and the agent recomputed them anyway; the database question is outside what
enola detects.

| Question | Exact, bare / enola | Cold $, bare / enola | Time, bare / enola |
|---|---|---|---|
| Import cycles in web and Android | 5 / 5, 5 / 5 | 0.56 / 0.40 | 94 s / 49 s |
| Screens an endpoint change breaks | 5 / 5, 5 / 5 | 0.88 / 0.83 | 146 s / 100 s |
| Packages in the zone of pain | 5 / 5, 5 / 5 | 0.28 / 0.34 | 27 s / 35 s |
| The most important wasted database work | 5 / 5, 5 / 5 | 0.60 / 0.91 | 55 s / 99 s |

- **Cycles.** Both arms were exact every time. The bare arm wrote and ran its own import
  resolvers for TypeScript and Kotlin; with enola the same answer cost about 30% less and
  took half the time.
- **Screens an endpoint change breaks.** The chain from the handler to its screens is
  short and named the same way in every app, so both arms followed it. enola gave the
  endpoints and their client files at once, which took about a third off the time.
- **Package metrics.** With every definition spelled out, the question became one Go
  program over `go list` and `go/parser`. The enola arm called `package_metrics`,
  received the correct numbers, then wrote that program anyway and answered from its own
  output.
- **Wasted database work.** The answer is one uncached query that middleware runs on
  every request. enola's performance analysis looks for queries inside loops, so it had
  nothing to report, and the agent used it only to navigate.
- **Lookups, change plans, questions with a literal to search for, and questions a
  toolchain answers** (callers of a function, a package's dependents through `go list`,
  unused names through a grep loop), at one trial per arm in an earlier round: both arms
  were right, and bare was usually cheaper.

The screens and package-metrics questions were narrowed until they could be keyed
exactly: one handler with a short chain, one metric over one Go module. What was left was
the size of a script, so those runs measure whether the agent can write that script.

### Two open questions, asked as typed

Asked across all five repositories with no answer format and no key, three trials per
arm, and judged by tracing every claim in every reply to the code. Medians per session.

| Question | Cold $, bare / enola | Time, bare / enola | Steps, bare / enola |
|---|---|---|---|
| The most important performance issue for the whole system | 0.53 / 0.58 | 68 s / 60 s | 13 / 10 |
| The most important thing to do in each repository, from package metrics | 1.00 / 0.56 | 194 s / 59 s | 24 / 7 |

Every performance claim in both arms held. The bare arm gave the same answer three times,
the per-request query above. The enola arm gave two different real issues, ranked by the
client traffic enola reports, and neither arm found the other's. On package metrics the
enola arm's pick per repository barely moved while the bare arm's varied, but the bare
arm, reading source, found true things no tool reports. In two of three trials the enola
arm explained an import cycle by a barrel file that does not exist.

### What this shows, and what it does not

- **One system.** Go, Next.js, Swift and Kotlin. A Rails monolith or a Python backend
  with a Vue frontend exercises other extractors, of other maturity, and a missed idiom
  becomes a wrong answer.
- **One model.** Opus 5.5, chosen as the strongest model in daily use rather than one
  that would flatter enola. Smaller models are not measured yet.
- **The enola arm was told to use enola.** Whether an agent reaches for it unprompted is
  not measured here.
- **A deterministic analysis does not make a deterministic agent.** enola returned the
  same result in every trial, and the agent still mistyped, omitted or recomputed parts
  of it.

About 150 sessions and $125 of model usage, priced cold. The full write-up, with each
question's wording: [Claude Code benchmark: Enola on code the model had never
seen](https://enola.tech/blog/benchmarking-enola-on-code-the-model-never-saw).

## What is measured, and why these six things

A code graph for agents is usually benchmarked on retrieval: how few tool calls,
how few tokens, how fast the agent finds the right file. The field measurement above
answers those questions for the workload that occurred. The fixed corpus below asks
whether the structural verdict an agent receives can be believed.

Its timings are the engine's own snapshot `duration` rather than wall-clock. The sweep
passes `--memstats`, which forces a full GC before reporting its figures, and on a
kernel-sized heap that is a few hundred milliseconds of process time no user pays.

The questions below are the ones that decide whether a *structural verdict* about
your change can be believed:

| | The question | Why it cannot be skipped |
|---|---|---|
| **1** | Does the same commit produce the same graph? | A diff between two snapshots is meaningless if the snapshots are not reproducible |
| **2** | When something regresses, is exactly that reported? | A gate that also reports pre-existing problems gets switched off in a week |
| **3** | Of the cross-repo edges that exist, how many are found — and are the misses shown? | A resolved-only count is advertising |
| **4** | Does it finish on a real repository? | If it can't parse a real codebase, there's nothing to report on |
| **5** | Does an agent, given the tool, actually avoid shipping the regression? | The honest test of the whole idea |
| **6** | Does a framework question get the right answer, and does a miss say why? | A graph can grow while the answer an agent asked for gets worse |

Axes 1–3 have no counterpart in a retrieval benchmark, because you cannot report a
delta without a baseline, and you cannot report a miss without knowing what a hit
would have been.

## The corpus

107 repositories, 502,122 source files parsed of 829,788 seen, 9,210,440 facts
carrying 26 distinct language tags (Ansible, C, C++, C#, Dart, F#, Go, HCL, Java,
Kotlin, Markdown, PHP, Python, Razor, Ruby, Rust, Scala, SQL, Stimulus, Swift,
TypeScript, VB.NET, XAML, gRPC, OpenAPI, and enola's own intent pages). Public open-source only: every row is a
repository you can clone and re-run.

**107 of 107 reproduce** — identical `snapshot_id` and identical
`facts.jsonl` hash across a cold run and two warm ones, i.e. across cache states.

**And 81 of 81 reproduce across separate sweeps**, measured when 0.4.0 was validated:
that sweep ran twice, forty minutes apart, and every repository's `facts.jsonl` was
byte-identical between the two runs. That is a stronger claim than the one above, which compares
three runs inside a single sweep: it holds across process lifetimes and a machine
whose memory and page cache had moved on. One caveat stated rather than hidden —
`snapshot_id` covers the fact stream, not the receipt's `file_hashes` list, so this
result says nothing about that field's stability.

This is one sweep, not a sum of several, so every measured number in sections 1 to 5
comes from the same run of the same binary: the sweep pinned as the v474 baseline on
2026-10-27, on enola at `b085875f`. The ratchet and Laravel tables and section 7 come
from a full `bench-all.sh` run the same day on `07f39aac`, two commits earlier, whose
`facts.jsonl` is byte-identical on every repository but enola itself. A passage that names an earlier version
describes that version.

**Seven repositories joined for this sweep**, the ones [section 7](#7-framework-questions-are-the-answers-right)
grades framework answers on: medusa, nhost, directus, vite, tailwindcss,
cypress-realworld-app and vite-tailwind-graphql, 226,076 facts between them. Against the
v465 pin, 51 of the 99 repositories both hold produce a byte-identical `facts.jsonl`, and
12 more keep their fact counts while edges inside them move. Of the 36 whose counts move,
the large drops are one change: Cypress specs (`*.cy.*`) are test code now, so spartacus
loses 1,129 facts and taiga-ui 518 from files that were indexed as application code. The
rises are mostly Tailwind tokens and the components that use them (supabase +490), and
imports through a Vite alias that used to be recorded as external packages (airflow's
UI: 1,137 imports now reach its own modules).

The five before them, VS Code, Django, OkHttp, Gin and Alamofire, joined at v465 as the
repositories a published code-graph comparison measured on.

**Ruby grew from four repositories to thirteen**, which is what surfaced the Rails
work described below. The four it had — gitlab, discourse, chatwoot, solidus — shared
one shape between them, a single application with a single root `config/routes.rb`,
and three defects hid behind that. See [the Ruby rows](#ruby--rails).

**PHP grew from eight repositories to twelve**, and the four are Laravel applications.
The one Laravel repository it had, cachet, is since 3.x an application shell whose
controllers and models live in a vendored package, so nothing in the corpus could show
whether a Laravel route reaches a model. See [the Laravel rows](#php--laravel).

**TypeScript is the largest language after C**: 1,920,920 facts against C's
2,075,309 (almost all of it the Linux kernel). It was 1,746,628 at v465, and the seven
new repositories bring 174,369 of the rise. It was 1,199,067 at v423b: VS Code brings
428,215, and the rest of the rise, to 1,318,413 without it, is interface members and
class fields now declared as symbols, so a call through a typed service lands on one. It
was 781,437 at v224, when it overtook the .NET family (now 1,135,557 facts tagged C#,
VB.NET, F#, Razor/Blazor or XAML) and C# alone (966,849); that jump was the v253 detection
change, not a corpus change. Dart is unchanged at 792,440. Every figure in this paragraph counts
facts by **language tag**, not by repository: a per-language section below totals
everything its repositories contain, which is a larger and different number.
See [the .NET rows](#net) below.

| Repository | Language | Files parsed | Facts | Cold | Warm |
|---|---|---|---|---|---|
| linux | c | 57,480 | 2,068,665 | 143.8s | 44.9s |
| gitlab | ruby | 55,951 | 551,411 | 40.2s | 24.4s |
| runtime | csharp | 23,686 | 549,895 | 39.6s | 22.7s |
| dart-sdk | dart | 16,892 | 454,561 | 47.8s | 9.9s |
| vscode | typescript | 11,974 | 450,752 | 20.7s | 11.7s |
| rust | rust | 37,669 | 389,206 | 15.4s | 9.6s |
| roslyn | csharp | 17,511 | 378,250 | 15.3s | 11.2s |
| shopware | php | 19,908 | 239,853 | 8.6s | 5.8s |
| spark | scala | 5,749 | 220,000 | 9.2s | 7.1s |
| grafana | go | 11,379 | 214,407 | 10.4s | 5.7s |
| flutter | dart | 7,886 | 211,557 | 7.8s | 5.8s |
| thingsboard | java | 6,621 | 169,885 | 5.3s | 3.6s |
| discourse | ruby | 13,258 | 145,188 | 9.4s | 5.6s |
| flutter-packages | dart | 3,971 | 136,376 | 6.0s | 4.2s |
| gauzy | angular | 8,915 | 132,295 | 6.6s | 3.6s |
| openproject | ruby | 11,289 | 111,513 | 8.9s | 6.0s |
| medusa | typescript | 10,037 | 107,447 | 12.5s | 7.1s |
| nextcloud-server | php | 6,028 | 104,598 | 4.1s | 2.2s |
| pekko | scala | 2,296 | 97,632 | 2.7s | 2.3s |
| airflow | python | 13,642 | 95,943 | 9.3s | 6.1s |
| supabase | typescript | 7,425 | 91,209 | 8.3s | 2.7s |
| bitwarden-clients | typescript | 5,090 | 87,163 | 4.0s | 2.4s |
| angular | angular | 7,126 | 86,712 | 4.4s | 2.4s |
| ente | dart | 3,757 | 85,433 | 3.8s | 2.1s |
| deno | rust | 4,817 | 85,379 | 4.8s | 2.3s |
| dubbo | java | 4,374 | 82,958 | 2.2s | 2.0s |
| spartacus | angular | 7,051 | 77,665 | 4.8s | 2.7s |
| superset | python | 4,324 | 70,488 | 4.8s | 2.9s |
| fsharp | fsharp | 1,916 | 69,766 | 2.2s | 1.6s |
| avalonia | xaml | 3,883 | 64,946 | 2.6s | 1.5s |
| orchardcore | razor | 7,360 | 61,490 | 5.9s | 1.8s |
| nhost | typescript | 4,655 | 60,240 | 4.6s | 2.2s |
| lila | scala | 2,345 | 58,332 | 2.6s | 1.6s |
| appflowy | dart | 2,510 | 57,213 | 1.9s | 1.5s |
| cal.com | typescript | 4,996 | 56,171 | 3.5s | 1.6s |
| dbt-core | rust | 1,507 | 49,145 | 1.9s | 1.2s |
| peertube | angular | 2,832 | 45,869 | 2.0s | 1.1s |
| wordpress | php | 2,775 | 45,863 | 3.1s | 1.3s |
| gitea | go | 5,540 | 44,261 | 2.5s | 1.4s |
| gmsh | cpp | 1,765 | 43,683 | 2.0s | 1.0s |
| powershell | csharp | 1,302 | 43,080 | 2.2s | 1.2s |
| bitwarden-server | csharp | 3,707 | 42,263 | 2.0s | 1.3s |
| immich | dart | 2,172 | 40,360 | 1.9s | 1.0s |
| chatwoot | ruby | 4,010 | 40,292 | 2.4s | 1.4s |
| mudblazor | razor | 3,186 | 38,906 | 1.6s | 0.8s |
| directus | vue | 2,907 | 38,814 | 2.1s | 1.2s |
| rails | ruby | 2,673 | 37,935 | 2.0s | 1.5s |
| taiga-ui | angular | 2,921 | 35,894 | 1.6s | 1.0s |
| angular-components | angular | 2,276 | 33,249 | 1.6s | 1.0s |
| flutterfire | dart | 1,188 | 33,037 | 1.7s | 1.0s |
| mastodon | ruby | 3,444 | 32,537 | 2.5s | 1.4s |
| cognee | python | 2,326 | 31,609 | 3.9s | 2.1s |
| saleor | python | 2,501 | 30,923 | 2.3s | 1.5s |
| flarum | php | 2,735 | 30,459 | 1.1s | 0.7s |
| mcp | csharp | 2,318 | 28,328 | 1.5s | 0.9s |
| zio | scala | 1,137 | 28,294 | 1.1s | 0.8s |
| jellyfin | csharp | 1,892 | 27,633 | 1.0s | 0.6s |
| koel | php | 2,227 | 27,417 | 1.0s | 0.6s |
| firefly-iii | php | 1,600 | 25,088 | 1.2s | 0.5s |
| pekko-http | scala | 1,028 | 23,471 | 0.8s | 0.6s |
| spotube | dart | 457 | 22,086 | 0.6s | 0.4s |
| monica | php | 1,695 | 20,969 | 1.4s | 0.5s |
| drift | dart | 917 | 20,457 | 0.9s | 0.6s |
| cognee-rs | rust | 1,015 | 17,736 | 0.7s | 0.4s |
| files | xaml | 1,286 | 17,410 | 0.8s | 0.5s |
| django | python | 791 | 17,153 | 3.0s | 2.0s |
| bookstack | php | 1,118 | 16,675 | 1.0s | 0.5s |
| okhttp | kotlin | 685 | 16,404 | 0.7s | 0.4s |
| fastlane | ruby | 1,073 | 15,888 | 1.1s | 0.6s |
| openwhisk | scala | 484 | 15,320 | 0.6s | 0.4s |
| tokio | rust | 819 | 14,684 | 0.5s | 0.3s |
| enola | go | 732 | 14,060 | 10.1s | 0.5s |
| http4s | scala | 474 | 13,874 | 0.6s | 0.4s |
| crates-io | rust | 944 | 13,393 | 0.7s | 0.4s |
| solidus | ruby | 2,036 | 12,954 | 1.2s | 0.7s |
| vite | typescript | 1,500 | 12,332 | 1.0s | 0.4s |
| ngrx | angular | 1,078 | 11,687 | 0.7s | 0.4s |
| excalidraw | typescript | 1,211 | 11,676 | 1.3s | 0.4s |
| dashboard | angular | 639 | 10,859 | 0.9s | 0.3s |
| localsend | dart | 449 | 10,740 | 0.5s | 0.3s |
| rubygems.org | ruby | 1,212 | 6,895 | 0.6s | 0.3s |
| gitbucket | scala | 238 | 6,557 | 0.3s | 0.2s |
| nextcloud-collectives | php | 414 | 5,965 | 0.4s | 0.2s |
| isowords | swift | 387 | 5,838 | 0.3s | 0.2s |
| csharp-sdk | csharp | 519 | 5,480 | 0.4s | 0.2s |
| nowinandroid | kotlin | 356 | 5,407 | 0.3s | 0.3s |
| tailwindcss | typescript | 304 | 5,320 | 0.7s | 0.2s |
| getdp | cpp | 184 | 4,754 | 0.6s | 0.1s |
| alamofire | swift | 113 | 4,093 | 0.3s | 0.1s |
| elk | vue | 398 | 3,631 | 0.2s | 0.1s |
| eshop | csharp | 585 | 3,631 | 0.3s | 0.2s |
| lobsters | ruby | 541 | 2,904 | 0.4s | 0.2s |
| ngx-admin | angular | 254 | 2,880 | 0.2s | 0.1s |
| grape | ruby | 201 | 2,421 | 0.2s | 0.1s |
| activeadmin | ruby | 295 | 2,416 | 0.2s | 0.1s |
| trading | scala | 128 | 2,261 | 0.2s | 0.1s |
| nextcloud-contacts | php | 177 | 2,051 | 0.3s | 0.1s |
| ng-alain | angular | 139 | 1,876 | 0.2s | 0.1s |
| cypress-realworld-app | typescript | 134 | 1,859 | 0.2s | 0.1s |
| devise | ruby | 177 | 1,383 | 0.1s | 0.1s |
| gin | go | 67 | 1,261 | 0.7s | 0.1s |
| giraffe | fsharp | 48 | 1,241 | 0.1s | 0.1s |
| grpc-web-example | grpc | 15 | 356 | 0.1s | 0.0s |
| sveltekit-realworld | svelte | 45 | 245 | 0.1s | 0.0s |
| cachet | php | 30 | 161 | 0.1s | 0.1s |
| orocrm | php | 8 | 124 | 0.1s | 0.0s |
| vite-tailwind-graphql | vue | 10 | 64 | 0.1s | 0.0s |

### Ruby / Rails

Thirteen repositories, 96,160 files parsed, 963,737 facts of which **554,238 are Ruby**.
All thirteen reproduce.

| Repository | Files parsed | Facts | Ruby facts | rails routes | grape routes |
|---|---:|---:|---:|---:|---:|
| gitlab | 55,951 | 551,411 | 305,799 | 2,059 | 1,554 |
| discourse | 13,258 | 145,188 | 69,561 | 1,552 | 0 |
| openproject | 11,289 | 111,513 | 74,088 | 1,678 | 389 |
| mastodon | 3,444 | 32,537 | 17,319 | 777 | 0 |
| rails | 2,673 | 37,935 | 33,622 | 32 | 0 |
| chatwoot | 4,010 | 40,292 | 14,345 | 690 | 0 |
| solidus | 2,036 | 12,954 | 11,617 | 690 | 0 |
| rubygems.org | 1,212 | 6,895 | 6,286 | 239 | 0 |
| lobsters | 541 | 2,904 | 2,742 | 246 | 0 |
| grape | 201 | 2,421 | 1,929 | 0 | 5 |
| fastlane | 1,073 | 15,888 | 13,684 | 0 | 0 |
| devise | 177 | 1,383 | 1,305 | 0 | 0 |
| activeadmin | 295 | 2,416 | 1,941 | 0 | 0 |

**The four zero rows are the result, not a gap.** `fastlane` is scale Ruby with no Rails
at all — the control for whether a Rails change breaks plain Ruby. `devise` and
`activeadmin` are engines whose only `routes.rb` files sit under `lib/` and *define*
routing DSL rather than declaring routes. `grape` yields 5 routes from 1,596 verb sites
for the same reason: `lib/grape/**` implements the DSL, and the specs that exercise it
are excluded as tests. A library defines a DSL and does not use one, so validating a
route extractor against the library that implements it measures the wrong thing — the
lesson the Scala corpus learned, applied here from the start. gitlab and openproject are
what grade the extractor; `rails`' own 23 routes come from `activestorage` and
`actionmailbox`, the two gems that ship a real route file.

#### What the nine new repositories exposed

The corpus was four repositories until this run, and all four shared one shape — a single
application with a single root `config/routes.rb`. Three defects hid behind that, and a
fourth hid behind the assumption that a route file contains only route DSL:

| | before | after |
|---|---:|---:|
| solidus route files read | 0 / 5 | **5 / 5** |
| solidus rails routes | 0 | **735** |
| discourse route files read | 1 / 26 | **26 / 26** |
| discourse rails routes | 1,195 | **1,729** |
| gitlab grape routes | 0 | **1,554** |
| openproject grape routes | 0 | **389** |
| discourse layer violations | 426 | **27** |

A Rails route table is not one file. Routes are now collected from every
`<dir>/config/routes.rb` and every `.rb` below a `config/routes/` directory at any depth,
each parsed under the prefix it is actually served at — `draw(:pkg)` followed
transitively, and `mount SomeEngine, at: '/x'` resolved to the directory owning that
constant so the engine's whole route table is parsed below `/x`. Routes also carry a
handler and a `handled_by` edge to the controller action, without which a Rails route is
an isolated node and a controller reached only through the route table reads as dead
code. The walk descends through Ruby control flow, because a route file *is* Ruby and
real ones guard whole blocks with `if`/`unless` — Rails' own
`activestorage/config/routes.rb` is a `draw` block closed by an `if` modifier, and five
route files across the corpus parsed cleanly and produced nothing.

The layer row is not an extractor change. discourse is a Rails backend beside an Ember
frontend, the ember-octane pattern wins the repo on confidence, and layer matching is by
path segment with no notion of language — so with `lib` in Ember's level-0 util layer,
every Ruby `lib/` and `plugins/*/lib/*` directory became the innermost layer and each
model or service it legitimately called became a violation. 397 of 426 were that.

**Measured parse coverage.** Every `.rb`/`.rake` file in all thirteen repositories,
parsed with the pinned `tree-sitter-ruby` v0.23.1 and its error nodes attributed to the
nearest enclosing definition: **87,729 files, 0.04% with any error, 0.01% losing a
type** — the column that matters, and materially cleaner than the Scala corpus. The
conclusion that bought is the important one: the grammar was never the problem, and
every gap above was in extraction logic.

### Dart / Flutter

Ten repositories, 40,199 files, 1,071,820 facts of which **792,041 are Dart**. All ten
reproduce, and **all ten parse with zero errors**, measured for parse coverage before the
extractor was written.

| Repository | Files parsed | Facts | Dart facts | Cold | Warm |
|---|---|---|---|---|---|
| dart-sdk | 16,892 | 454,561 | 394,478 | 47.8s | 9.9s |
| flutter | 7,886 | 211,557 | 143,735 | 7.8s | 5.8s |
| flutter-packages | 3,971 | 136,376 | 96,074 | 6.0s | 4.2s |
| ente | 3,757 | 85,433 | 41,595 | 3.8s | 2.1s |
| appflowy | 2,510 | 57,213 | 40,484 | 1.9s | 1.5s |
| immich | 2,172 | 40,360 | 14,479 | 1.9s | 1.0s |
| flutterfire | 1,188 | 33,037 | 13,645 | 1.7s | 1.0s |
| spotube | 457 | 22,086 | 21,272 | 0.6s | 0.4s |
| drift | 917 | 20,457 | 18,703 | 0.9s | 0.6s |
| localsend | 449 | 10,740 | 7,576 | 0.5s | 0.3s |

dart-sdk is the outlier on cold time and not because of its Dart: it carries 1,138
C/C++ sources and a large `runtime/` tree, so the C/C++ extractor does substantial work
on it too. Its warm run is 4.8x faster than cold.

Two rows are cross-repo clusters rather than single applications. **immich** pairs a
Flutter client with a TypeScript server and **ente** pairs one with a Go server; split
into their halves, ente's client resolves 167 of 168 outbound call sites against its
own backend (see [Cross-repo resolution](#3-cross-repo-resolution-misses-included)).

**flutter-packages, flutterfire, drift and spotube are almost pure Dart** (96,074 of
96,081 facts; 13,645 of 13,677; 18,703 of 18,703; 21,272 of 21,528), which makes them
the rows where a Dart extraction regression shows up undiluted.

### .NET

Fourteen repositories, 69,199 files, 1,332,319 facts — everything those repositories
contain, markdown and configuration included. Counted by language tag instead, C#,
VB.NET, F#, Razor and XAML together carry 1,135,557 of them; the corpus section above
ranks the languages on that basis. All fourteen reproduce.

| Repository | Files parsed | Facts | Cold | Warm |
|---|---|---|---|---|
| runtime | 23,686 | 549,895 | 39.6s | 22.7s |
| roslyn | 17,511 | 378,250 | 15.3s | 11.2s |
| fsharp | 1,916 | 69,766 | 2.2s | 1.6s |
| avalonia | 3,883 | 64,946 | 2.6s | 1.5s |
| orchardcore | 7,360 | 61,490 | 5.9s | 1.8s |
| powershell | 1,302 | 43,080 | 2.2s | 1.2s |
| bitwarden-server | 3,707 | 42,263 | 2.0s | 1.3s |
| mudblazor | 3,186 | 38,906 | 1.6s | 0.8s |
| mcp | 2,318 | 28,328 | 1.5s | 0.9s |
| jellyfin | 1,892 | 27,633 | 1.0s | 0.6s |
| files | 1,286 | 17,410 | 0.8s | 0.5s |
| csharp-sdk | 519 | 5,480 | 0.4s | 0.2s |
| eshop | 585 | 3,631 | 0.3s | 0.2s |
| giraffe | 48 | 1,241 | 0.1s | 0.1s |

The corpus is deliberately split so each mechanism has a control:

| Repo | What it is the control for |
|---|---|
| jellyfin | ASP.NET Core **attribute** routing — 420 routes |
| eShop | **minimal APIs** with `MapGroup` prefixes, EF Core, .NET Aspire |
| OrchardCore | **conventional** MVC routing, Razor Pages — 94% of its controllers carry no `[Route]` |
| roslyn | **VB.NET at scale** — its VB compiler is the largest VB codebase in the open |
| fsharp, Giraffe | **F#**; Giraffe ships no `.csproj` at all |
| MudBlazor | **Blazor** components — 1,987 `.razor` against 1,154 `.cs` code-behind |
| Avalonia, Files | **XAML** — Avalonia at scale, Files as a real WinUI 3 MVVM app |
| bitwarden-server | EF Core + Dapper; the backend half of the cross-repo cluster |
| runtime, PowerShell, mcp, csharp-sdk | scale, `partial`-heavy code, a 148-project monorepo, a library serving no HTTP |
| lila | Play `conf/routes` — one file declaring 923 endpoints, with four included sub-routers |
| openwhisk, pekko-http | a Pekko HTTP **application** against the library that defines the DSL |
| trading, http4s | the same pairing for http4s, and an effect-heavy Scala 3 codebase |
| spark, pekko | Scala **beside Java** in one tree — 1,355 and 582 `.java` in the same packages |

**giraffe is the row that matters most.** It produced **0 facts** before this work —
detection matched its `.slnx`, the extractor claimed the repository, and reported a
successful snapshot of 46 unread F# sources. A green snapshot of nothing is worse
than an unsupported language, because nothing in the output says so.

### Angular

Ten repositories, 33,231 files parsed, 438,986 facts. All ten reproduce; the largest
peaks at 427 MiB.

| Repository | Files parsed | Facts | Cold | Warm |
|---|---|---|---|---|
| gauzy | 8,915 | 132,295 | 6.6s | 3.6s |
| angular | 7,126 | 86,712 | 4.4s | 2.4s |
| spartacus | 7,051 | 77,665 | 4.8s | 2.7s |
| peertube | 2,832 | 45,869 | 2.0s | 1.1s |
| taiga-ui | 2,921 | 35,894 | 1.6s | 1.0s |
| angular-components | 2,276 | 33,249 | 1.6s | 1.0s |
| ngrx | 1,078 | 11,687 | 0.7s | 0.4s |
| dashboard | 639 | 10,859 | 0.9s | 0.3s |
| ngx-admin | 254 | 2,880 | 0.2s | 0.1s |
| ng-alain | 139 | 1,876 | 0.2s | 0.1s |

The corpus is split so that each mechanism has a control, the same way the .NET one is:

| Repo | What it is the control for |
|---|---|
| angular, components | the framework and the component library that **define** the decorators and the router — a library defines a DSL and does not use one |
| ngx-admin, ng-alain | classic **NgModule** applications; ngx-admin indexes in 0.4s and is the row to iterate against |
| taiga-ui | **standalone components and signals** throughout — 153 `standalone: true`, 213 `loadComponent`, and every route path written as an enum member |
| spartacus | library *and* application — 924 NgModules, and the control for a repository whose only `forRoot` calls are in specs |
| gauzy | the **Nx workspace** shape — 99 projects, a `ROUTES` provider factory, and workspace-package aliases |
| peertube, dashboard, gauzy | Angular against an **Express**, a **Go** and a **NestJS** backend |
| ngrx | a DI/state library with no application around it |

**peertube's 33.0s cold against a 1.8s warm** is the row to watch, and it is not the
Angular work: the same repository took 29.6s cold before any of it, and the cost is
its OpenAPI specification plus a 2,825-file TypeScript tree.

**Two rows correctly report almost nothing.** spartacus produces no routes at all —
its only `RouterModule.forRoot` calls live in `.spec.ts` files, and its routing is
configuration its consuming application applies. ngx-admin resolves 51% of its
template references because the other half name components from a third-party library
that lives in `node_modules`. Both numbers are reported with the cause named rather
than folded into a total.

### PHP / Laravel

Five repositories, of which four are new: 6,640 files parsed and 90,149 facts
across the four, 1,533 Laravel routes. All five reproduce, and the largest peaks at
94 MiB.

| Repository | Files parsed | Facts | laravel routes | with a handler | bound to a method | models | relations | endpoint reaches a model |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| firefly-iii | 1,600 | 25,088 | 640 | 640 | 633 | 51 | 191 | 117 of 200 |
| bookstack | 1,118 | 16,675 | 340 | 340 | 339 | 35 | 54 | 150 of 200 |
| monica | 1,695 | 20,969 | 334 | 333 | 325 | 67 | 206 | 163 of 200 |
| koel | 2,227 | 27,417 | 219 | 211 | 173 | 25 | 36 | 164 of 200 |
| cachet | 30 | 161 | 0 | 0 | 0 | 0 | 0 | 0 of 0 |

`bound to a method` counts the routes carrying a `handled_by` edge to a method the
repository declares. The last column is `enola endpoint` asked about a sample of each
application's routes, one route per query: it counts the ones whose chain runs from the
route through its handler to at least one Eloquent model. The rest stop at the
handler's dependencies, apart from 14 of the 800 that could not be asked about alone:
the query is a substring match, and a shorter path answered first. None stops at the
controller.

**cachet's zero row is the control.** Since 3.x it is an application shell: its
controllers, models and routes live in a package under `vendor/`, which is not indexed.

Each of the four is here for a shape the others lack:

| Repo | What it is the control for |
|---|---|
| koel | route files the skeleton does not name (`api.base.php`, `subsonic.php`), loaded by a computed file name; invokable controllers; services and repositories injected by constructor |
| bookstack | a root namespace that is not `App\`, controllers outside `Http/Controllers`, and an api file required inside a provider's prefixed group |
| monica | domain folders; actions run as `(new DestroyX)->execute($data)`; Laravel 11's `withRouting` |
| firefly-iii | the action array (`['uses' => 'Ctrl@method', 'as' => …]`) under `namespace` groups; repositories behind interfaces resolved from the container |

#### What the four new repositories exposed

Before they were cloned the Laravel reader was graded on a fixture written for it, and
it passed. On the first run over real applications:

- **koel yielded no routes at all.** Only `routes/web.php`, `api.php`, `console.php` and
  `channels.php` were read, and koel keeps its routes in `api.base.php` and
  `web.base.php`. Every PHP file under a `routes` directory is read now.
- **firefly-iii's 640 routes carried 5 handlers.** It writes every action as
  `['uses' => 'Controller@method', 'as' => 'name']`, a form the reader did not know.
- **Three of the four served their API one prefix away from where it was reported.**
  bookstack, monica and firefly-iii register `routes/api.php` under `/api` from a
  provider or from `bootstrap/app.php`, so 336 routes were stored without it. A routes
  file is registered under whatever loads it now. koel loads its files by a computed
  name, which is not resolved, and its paths are as written.
- **Resource routes were invented.** `apiResource(…)->only([…])` emitted all the
  actions regardless, every parameter was `{id}`, and koel's nested
  `albums.songs` was stored as the path `/api/albums.songs`.

**Why firefly-iii reaches a model on fewer than half its routes.** Its controllers
receive their repositories from the container inside a middleware closure
(`$this->repository = app(BudgetRepositoryInterface::class)`), and act on route-bound
models they only pass along. Neither states a type a call can be resolved through, so
the chain stops one step short. That is reported as a stop, not as an endpoint that
touches nothing.

**koel's 38 unbound handlers are mostly real.** It registers
`apiResource('albums', AlbumController::class)` with no `only`, so Laravel registers
`store` and `destroy` routes for a controller that declares neither. The routes exist
and would fail when called; enola reports them unbound instead of dropping them.

## 1. Reproducibility

Each repository was indexed **three times** — once cold, twice warm — and the
receipt's `snapshot_id` and the SHA-256 of `facts.jsonl` were compared across all
three. Running cold then warm is the point: it tests that a cached run and a
from-scratch run agree, not merely that the same code path repeats itself.

> **107 of 107 repositories in this sweep produced a byte-identical `snapshot_id` and a
> byte-identical `facts.jsonl` across all three runs — 321 runs, 9,210,440 facts,
> zero drift.** `insights.json` is byte-stable on 102 of the 107; the five where it is
> not (cognee, superset, airflow, cognee-rs, spark) differed in the previous sweeps too,
> and their facts do not. grafana left that list at v474: of two equally short paths
> from a loop to a query, the `query-loops` explainer named whichever a map iteration
> reached first, and ente and nhost had the same tie.

Two defects had to be fixed for that sentence to be true again, and both had the same
shape: **a snapshot must not depend on whether a previous snapshot exists.** A
markdown link was resolved by stat-ing the filesystem, so this repository's own docs
— which cite paths under its output directory — linked to nothing on a cold run and
to something on the next. And the output directory was ignored only at the repository
root, so a cluster config that snapshots subdirectories left an `.enola` in each and
enola indexed its own `llm_context.md` as a source document. Ten repositories in this
corpus carried such facts, `linux` and `wordpress` among them. Links now resolve
against the walked file set, and the output directory is ignored at any depth.

The 0.4.0 validation ran the whole sweep **twice**, forty minutes apart, and compared
the two: all 81 `facts.jsonl` were byte-identical between sweeps as well — 486 runs in
total. That check belongs to a release rather than to every sweep, so it is the one
number here carried forward. Three runs inside one sweep share a process lifetime and a warm page cache;
two sweeps do not, so this rules out a class of drift the in-sweep check cannot see.

What it does not cover is worth naming. `snapshot_id` hashes the fact stream, the
version and the config — **not** the receipt's `file_hashes` list, which records a
content hash per walked file and is what `enola check` and `plan` use to judge whether
a snapshot still describes the working tree. A discrepancy confined to that list would
leave every number above unchanged.

This is the property everything else rests on. `snapshot_id` is
`sha256(facts ‖ enola version ‖ config hash)`, not a UUID, so two runs on the same
commit are provably the same graph. That's what lets `compare_receipts` refuse to
diff snapshots that are not comparable instead of reporting churn as your change.

An ID a third party can re-derive is the point rather than a hygiene property: it is
what makes a graph a value you can keep and compare, instead of a state you are
currently in. See [SNAPSHOTS.md](SNAPSHOTS.md).

## 2. Delta precision — the ratchet

The claim is that enola reports **what your change did** and stays silent about
everything that was already wrong. That is testable directly: pin a baseline, put
the repository through four states, and see what each one is graded as.

For each repository below, files were **added, never edited**, so every revert is a
delete. The cycle case adds two new modules that import each other.

**The policy these runs enforced was `--fail-on=cycles`**, stated because it has to be:
`enola check` fails nothing unless a policy names it, so the FAIL column is what that
flag does, not what a bare run does. The column that matters for precision is the same
either way — the delta itself, and the fact that it is exactly one finding.

| Repository | Language | Pre-existing findings | No change | Benign addition | Injected cycle | Reverted |
|---|---|---|---|---|---|---|
| chatwoot | Ruby | 330 | PASS · +0 facts | PASS · +3 facts | **FAIL · 1 regression** | PASS · +0 |
| gauzy | TypeScript | 323 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| solidus | Ruby | 306 | PASS · +0 facts | PASS · +3 facts | **FAIL · 1 regression** | PASS · +0 |
| gitea | Go | 259 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| superset | Python | 243 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| cognee | Python | 235 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| jellyfin | C# | 207 | PASS · +0 facts | PASS · +3 facts | **FAIL · 1 regression** | PASS · +0 |
| drift | Dart | 206 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| localsend | Dart | 194 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| enola | Go | 187 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| crates-io | Rust | 185 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| excalidraw | TypeScript | 174 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| lobsters | Ruby | 163 | PASS · +0 facts | PASS · +3 facts | **FAIL · 1 regression** | PASS · +0 |
| eshop | C# | 146 | PASS · +0 facts | PASS · +3 facts | **FAIL · 1 regression** | PASS · +0 |
| gitbucket | Scala | 142 | PASS · +0 facts | PASS · +3 facts | **FAIL · 1 regression** | PASS · +0 |
| elk | TypeScript | 123 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| nowinandroid | Kotlin | 88 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| ngx-admin | TypeScript | 38 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| sveltekit-realworld | TypeScript | 6 | PASS · +0 facts | PASS · +2 facts | **FAIL · 1 regression** | PASS · +0 |
| cachet | PHP | 5 | PASS · +0 facts | PASS · +3 facts | **FAIL · 1 regression** | PASS · +0 |

Read the columns as four separate claims, all of which hold on all twenty repositories, 20 of 20:

- **No change → +0 facts, +0 edges, PASS.** Exactly zero on all twenty repositories.
  That's what makes a PASS or FAIL on a real change something you can rely on.
- **Benign addition → PASS**, with the delta naming exactly the 2–3 facts added.
  A new leaf module isn't a structural regression, so there's nothing to report.
- **Injected cycle → FAIL, exactly 1 regression** — out of **3,560 pre-existing
  findings across these repositories**, up to 330 in a single one. None of them was
  repeated. The ratchet holds.
- **Reverted → PASS again**, +0 facts. The verdict is a function of the tree, not
  of history.

Ten languages, one behaviour. A regression is not detected by pattern-matching a
language; it is a cycle in the module graph, computed by Tarjan's SCC over the
resolved import edges, at confidence `1.0`.

**Swift is deliberately absent**, and the reason is a property of the language rather
than a limit of enola. A Swift module is a *declared* SPM target, so two added
directories that import each other form no edge and no cycle, verified: the modules
and the symbols appear, the cycle does not. Injecting one would mean editing
`Package.swift`, and then "reverted" would be a file restore rather than a delete,
which is exactly the property that makes the no-change column worth reading.

### What is eligible to fail at all

Across the corpus enola produced **20,228 findings**. Broken down:

| Explainer | Findings | Class |
|---|---|---|
| dead-code | 4,223 | candidate |
| performance | 3,955 | heuristic |
| god-class | 2,028 | statistical outlier |
| hotspots | 1,626 | statistical outlier |
| **cycles** | **1,535** | **structural fact + heuristic, see below** |
| exported-surface | 1,320 | candidate |
| complexity-outliers | 1,236 | statistical outlier |
| package-metrics | 1,136 | exact metric, judged by a threshold |
| layers | 837 | heuristic |
| dead-methods | 833 | candidate |
| dependency-depth | 714 | statistical outlier |
| domain | 394 | heuristic |
| query-loops | 292 | heuristic |
| import-closure | 78 | measured cost |
| entry-points | 13 | candidate |
| vendored-candidates | 5 | candidate |
| intent | 3 | declared-intent verdict |

**Most of it comes from `dead-code`, `performance` and `package-metrics`,** which ask
about the whole repository. They became defaults at v268 and produce 9,314 of the
20,228. None of them reaches the `1.00` floor: a candidate, an estimate
and a threshold over an exact metric are all opinions about the code, however exact
the numbers under them.

**At v224 the total was 12,027, down from 29,633 at v197, and 23,194 of that drop was
one explainer.** hotspots was roughly 80% of every finding enola produced, which meant
the ranking of one explainer decided what a reader saw first, and the other nine were
buried underneath it. It reports at most its top 20 per repository, highest score
first, ties broken by name so the cut is deterministic. The outlier threshold already
kept the set small on most repositories, so the cap only bites where the volume was
noise.

Two smaller corrections travelled with it. hotspots scores fan-in × fan-out, and
`has_method` edges (a type to the methods it declares) were being counted as calls
*out of* the type, so a large class read as a pinch point for being large: one
449-line importer whose body is 102 one-line delegations and exactly one call out
ranked in a monolith's top 20 as "it calls out to 104 others". And `dead-methods`
arrived at v224 as the largest row, 2,847 entries; it is 833 here. Every entry is a
candidate, never a verdict, scoped to surfaces whose callers the graph can see.

**None of these fail anything by default** — `enola check` names no explainer unless
you do. The number worth reading is how much of the corpus is even *eligible* to fail
at the `1.00` floor, whatever you name: only findings enola proves reach it, and every
statistical row above caps below it by construction.

**The cycles row is not all one thing, and the difference is instructive.** The
explainer emits a load-order cycle at confidence `1.0` and, separately, a *highly
coupled module cluster* at `0.4` — mutual references between directories in an
autoloaded codebase (Rails, say), where constants resolve lazily and there is no
load-order defect to break. Both carry `source: cycles`, so counting the explainer
overstates what `--fail-on=cycles` would do.

Counting the corpus by confidence rather than by explainer: **1,702 of the 20,228
findings sit at the `certain` level**, the only one that reaches the `1.00` floor.
So **8.4% could fail a build even with every explainer named**, and the other 91.6%
are reported and let you through however you configure it.

That share was 3.16% at v197, 14.3% at v217, 10.8% at v224, and has stayed between
8.3% and 8.6% since v264. It moves in both directions for the same reason: it is a denominator. Capping
hotspots removed 23,194 findings that could never have failed anything, which raised
it; `dead-methods`, then `dead-code`, `performance` and `package-metrics` added
candidates and estimates, which lowered it. The count that can fail has moved far less, from 1,301 at
v224 to 1,702 here. The ratio is the design: the confidence floor keeps an estimate
from breaking a build even when someone asks it to, and the empty default keeps enola
from asking on your behalf.

## 3. Cross-repo resolution, misses included

enola reports both resolved and unresolved edge counts, so you know how many
dependents a service actually has.

### On the committed multi-repo fixtures

Eleven cross-language fixtures, each with a golden fact file the test suite asserts on:

| Fixture | Languages | Detected | Resolved | Unresolved | External | Edge confidence |
|---|---|---|---|---|---|---|
| ts_nest_multirepo | TypeScript ↔ TypeScript | 4 | 4 | 0 | 0 | verified |
| go_grpc_multirepo | Go ↔ Protobuf | 3 | 3 | 0 | 0 | verified |
| php_multirepo | PHP ↔ PHP | 2 | 2 | 0 | 0 | verified |
| py_fastapi_multirepo | Python ↔ TypeScript ↔ Go | 2 | 2 | 0 | 0 | — |
| py_grpc_multirepo | Python ↔ Protobuf | 1 | 1 | 0 | 0 | verified |
| multirepo | OpenAPI ↔ OpenAPI | 1 | 1 | 0 | 0 | verified |
| ts_express_multirepo | TypeScript ↔ JavaScript | 4 | 2 | **2** | 0 | verified |
| go_httpclient_multirepo | Go ↔ Go | 4 | 1 | **1** | 2 | probable |
| go_kafka_multirepo | Go ↔ Go (Kafka) | — | — | — | — | topic-matched |
| kotlin_swift_multirepo | Kotlin ↔ Swift | — | — | — | — | — |
| dart_multirepo | Dart ↔ Go | 3 | 3 | 0 | 0 | verified |
| **Total** | | **24** | **19** | **3** | **2** | |

The three unresolved edges are all deliberate, and each is a documented limit rather
than a bug: a path too generic to attribute (`/healthcheck`), a path no loaded server
serves, and a base URL injected from config with no literal binding.

`verified` versus `probable` is a real distinction the fact carries: an edge matched
through a declared contract (a `.proto` service, an OpenAPI `operationId`) is an
identity; an edge matched by comparing URL strings is a resemblance. See
[gRPC and OpenAPI](extraction/grpc-openapi.md).

### On a real public cluster

Nextcloud server plus two first-party apps, indexed into one graph
(101,673 + 1,812 + 5,458 facts):

```
  service      classification  detected  resolved  unresolved
  collectives  coverage_gap          2         0           2
  contacts     isolated              0         0           0
  server       coverage_gap         13         0          13
```

Zero of thirteen resolved. The report doesn't disguise it as success: `server` is
classified **`coverage_gap`**, not `isolated`, and the output says so in words —

> 1 service is classified `coverage_gap`: no resolved outbound edges, but enola DID
> detect outbound call sites. Do not read these as isolated — they are the case where
> "depends on nothing" and "enola could not tell" look identical from the graph alone.

Nextcloud apps reach the server through in-process PHP APIs rather than cross-repo
HTTP, so there is little here for the linker to match. The point of including it is
that a report which only showed clusters where enola does well would not be evidence
of anything.

### On a .NET cluster

`bitwarden/server` (C#) plus `bitwarden/clients` (TypeScript) — two independent
repositories, different build systems, talking over HTTP. This is the shape the
other clusters lack: cluster 1 is in-process PHP, and the gRPC-web example is one
tree.

```
  service            classification  detected  resolved  unresolved
  bitwarden-clients  connected            22         2          20
  bitwarden-server   coverage_gap         13         7           6
```

Before this corpus supported .NET, `bitwarden-server` read **`isolated` with 0
unresolved** — no `role=client` routes were emitted at all, so the linker had one
side of every edge and the server appeared to call nothing. `coverage_gap` with 6
unresolved is the honest replacement: enola found calls it cannot place.

Seven of thirteen resolve. The twenty unresolved on the client side are mostly
endpoints served by repositories not in this cluster.

### On a Dart/Flutter cluster

`ente-io/ente`'s Flutter client plus its Go server — a mobile app and the backend it
talks to, indexed as two repositories. A mobile client is the extreme case for
cross-repo linking: it serves nothing, imports nothing from the backend, and shares no
code with it, so an outbound call site is the *only* structural evidence the two belong
to one system.

```
  service   classification  detected  resolved  unresolved
  mobile    connected           169       167           0
  server    isolated              0         0           0
```

Before, this read `coverage_gap: 168 detected / 0 resolved / 167 unresolved` — and the
missing side was **not** the Dart one. ente's server is written with **gin**, which the
Go extractor did not read, so the client's 168 call sites had nothing to match against.
Adding gin recovers 359 server routes (one per registration outside test files, exactly)
and 112 of the client's 117 distinct paths match one exactly.

`server` reads `isolated` rather than `coverage_gap` because it makes no outbound calls
of its own — a backend at the edge of the graph, which is the correct classification and
not a gap.

### On the example a reader can run

[`examples/cross-repo/`](../examples/cross-repo/) — one command, two Go services:

```
==> Routes the api service actually serves
    (registered as "/orders/{id}" — stored at the composed runtime path)
    "/api/v2/orders"
    "/api/v2/orders/{id}"

  service  classification  detected  resolved  unresolved
  api      isolated              0         0           0
  web      connected             3         2           1
```

`api/server.go` registers the bare `/orders/{id}` inside a function that receives a
subrouter mounted at `/api/v2` in `main`. Neither function contains the served path;
the fact does. The web service's call resolves *because* that prefix was composed
across the call boundary. The one unresolved call is a deliberately dynamic URL,
so the demonstration proves its own limit in the same run.

## 4. Scale

| | |
|---|---|
| Largest repository indexed | **Linux kernel** — 57,480 files, **2,068,665 facts**, 143.8s cold / 44.9s warm |
| Largest .NET | dotnet/runtime — 23,686 files, 549,895 facts, 39.6s / 22.7s |
| Largest Ruby | GitLab — 55,951 files, 551,411 facts, 40.2s / 24.4s |
| Largest TypeScript | VS Code: 11,974 files, 450,752 facts, 20.7s / 11.7s |
| Largest Rust | rust-lang/rust — 37,669 files, 389,206 facts, 15.4s / 9.6s |
| Largest Scala | Spark — 5,749 files, 220,000 facts, 9.2s / 7.1s |
| Largest Go | Grafana — 11,379 files, 214,407 facts, 10.4s / 5.7s |
| Throughput | 1,400–39,000 facts/sec cold, depending on language |
| Parse errors, all 107 repositories | **32 files read only in part**, all Ruby (openproject 19, devise 9, fastlane 2, grape 2): tree-sitter error nodes the Ruby extractor counts and names since v413, rather than reading around them silently |
| Memory | peak heap per run is recorded by the sweep (`--memstats`) alongside time and hashes. The Linux kernel is the high-water mark at **6,606 MiB**; only six others exceed 1 GiB (GitLab 2,639, dotnet/runtime 2,137, VS Code 1,565, roslyn 1,476, dart-sdk 1,463, rust-lang/rust 1,246). No repository required tuning on this machine |

Times here are the engine's own `duration` for the snapshot. Warm runs are 1.1×–18.5×
faster than cold (median 1.7×, over the 82 repositories whose cold run exceeds 0.5s;
below that the timing is noise), from the per-file content-hash cache in
`snapshot.meta.json`. The process also writes its artifacts and records the
architecture-history revision after `duration` stops. A rerun whose `facts.jsonl` is
unchanged no longer streams the previous one to count a zero delta, so grafana's warm
run takes 7.0s of wall time against 5.4s of `duration`. These numbers establish that the graph the other four
sections rely on can actually be built on real code. enola isn't benchmarked on
speed as a competitive claim.

**The memory ratchet passes, and what it caught this time is worth recording.** Three
analyzers moved into enola at v268 and became defaults: `package-metrics`,
`dead-code` and `performance`. The first sweep after that breached a ceiling by six
times — `runtime` peaked at 13,130 MiB against 2,250 — and the harness is the only
reason anyone found out, because nothing about the analyzers' output was wrong.

Two measurements separated the cause from the noise. The first sweep ALSO reported
`roslyn` at 3,665s cold against 21.5s and a 30 GB peak, which was not a regression at
all: that sweep was run while a test suite and a linter were running on the same
machine, and `mem-ceilings.json` says in its own header to calibrate a ceiling
against the kind of run it grades. Re-run quiet, the timings came back and the memory
did not — which is what made the remaining figure a real finding rather than a
contended one.

The second was an A/B on rust-lang/rust, same binary, same repository, one explainer
at a time:

| explainer set | peak heap | wall |
|---|---|---|
| the nineteen that shipped before | 1,614 MiB | 27.2s |
| plus `package-metrics` | 1,535 MiB | 26.1s |
| plus `performance` | 1,535 MiB | 26.7s |
| plus `dead-code` | **8,798 MiB** | **51.0s** |

Two of the three were free. Profiling the third put all of it in one structure: a
reference index of `map[string]map[string]struct{}` holding **120,897,613 entries
over 1,625,276 relations**, 75 per relation, retaining 5 GiB. The multiplier is name
collision rather than volume — the index keys on folded short names, and
rust-lang/rust carries a UI test corpus declaring `Foo`, `T` and `Struct` in tens of
thousands of files, so 9,984 names held a thousand sources or more each.

The fix is that the index was answering a question nobody asked. Both of its readers
ask only whether *at least one* source satisfies a predicate, so it now stores a
count that saturates at two, the first source, and up to four sources with distinct
owners — which answers both EXACTLY rather than approximately, and the exactness is
what licenses throwing the rest away.

| | before | after |
|---|---|---|
| `runtime` peak (ceiling 2,250) | 13,130 MiB | **2,004 MiB** |
| `linux` peak (ceiling 8,000) | 6,725 MiB | **6,761 MiB** |
| rust-lang/rust peak | 10,435 MiB | **1,580 MiB** |
| rust-lang/rust wall | 51.6s | **27.4s** |

No ceiling was raised. The orphan set is byte-identical on four repositories checked
before and after (golf 1,399, dubbo 7,791, chatwoot 1,408, gmsh 12,194), which is
what makes it a performance fix rather than a behaviour change.

The lesson is the one the harness exists for: this cost was always there, and it was
invisible for as long as the analyzer shipped behind a licence that the open-source
benchmark never exercised. Making it a default is what measured it.

**Six extraction and store changes landed in this build, and what they moved is
allocation rather than wall clock.** None of them changes what is extracted, so none
bumped `cacheVersion`: every repository below carries a `facts.jsonl` hash identical
to the one the previous binary produced, and the route and framework counts in
section 5 moved by exactly one fact, in a repository whose own commit changed.
Allocated bytes is the metric this section leans on, because it reproduces: across
the controlled A/B pairs behind these figures it varied by under 0.05% between runs
of the same binary, while peak heap varies 23% to 28% run to run (`mem-ceilings.json`
says so in its own header) and wall clock varies with whatever else the machine is
doing.

| language | allocated, v268 | v271 | |
|---|---:|---:|---:|
| Scala | 446,985 MiB | 310,028 MiB | **-30.6%** |
| C | 94,430 MiB | 87,474 MiB | **-7.4%** |
| C++ | 2,364 MiB | 2,178 MiB | **-7.9%** |
| whole corpus | 755.7 GiB | 615.0 GiB | -18.6% |

The corpus row is the other three and almost nothing else: Scala accounts for 133.7 GiB
of the 140.7 GiB drop and C with C++ for 7.0 GiB. That is the honest shape of the
result, because those are the two extractors the work rewrote.

Scala was the outlier the sweep had never named. It parsed every file twice, once for
the declaration walk and again for the route and storage walks, and its import gates
copied each whole source to run a substring test. At 50.7 MiB allocated per parsed
file on Spark it sat about 100x above Java and Kotlin, which use the same parser
infrastructure and the same package index. Sharing one parse and matching on the bytes
took nine Scala repositories from 66.3s cold to 48.1s and cut their allocation by
30.6%, with a byte-identical `facts.jsonl` on every one. The C and C++ figure is a
second reading of the same mistake: the `#define` pre-pass split every source into a
string per line so that almost every line could be rejected on a leading `#`, which
measured 2.1x slower and 11.5x more allocation than scanning the bytes, over 3,000
kernel sources producing identical results.

One figure that is not in a table because it is not what the sweep records: the name
index is now a fact index rather than a slice per name, since 98.2% of the kernel's
1.9M names carry exactly one fact. Measured back to back on the same repository, that
is 1,421 MiB of steady heap against 1,343 and 1.9M fewer allocations. Steady heap is
what an MCP server holds for as long as it runs, so it outlives the snapshot the rest
of this section times.

**Two caveats, stated rather than hidden.** The corpus cold wall reads 757.3s against
658.7s, and most of that is not this work: Dart, C#, Ruby, Rust and PHP each moved 12%
to 21% in wall while their allocation stayed flat, which no change here can cause. Only
the Scala wall figure above is claimed, and it is claimed because a controlled
alternating-order A/B reproduced it. And the kernel's warm run reads 41.5s against the
previous sweep's 38.0s, which is machine state rather than code: measured back to back
four times on a quiet machine the same binary gives 38.3s to 39.1s. The sweep figure is
the one published, because a restricted re-measurement grades a different quantity than
a full sweep reaches after forty minutes of load, and `mem-ceilings.json` records what
conflating the two already cost once.

**A slowdown the gates did not catch, found while pinning v474.** The event and
Tailwind readers each re-parsed every file serially to blank its comments, and VS Code
went from 17.5s to 33.1s cold with byte-identical facts. Allocated bytes rose 14%, not
2x, because tree-sitter allocates on the C side, so the allocation figures this section
leans on did not show it. Event constants are now read from the tree the per-file pass
already holds and Tailwind classes in parallel; VS Code reads 19.2s against v465's 17.0s
measured back to back, and the comparison script now lists a cold run 1.5x and 3s
slower than its base.

Every ceiling in `mem-ceilings.json` is met at the v474 pin: linux at 6,606 MiB peak
against 8,000 and 459.7 mallocs per fact against 480, runtime at 2,137 against 2,700 and
715.9 against 740.


## 5. What the extractors see

Across the corpus enola extracted **46,058 routes** (33,636 server, 12,422 client) and
recognised **56 distinct frameworks** without configuration:

```
rails 7963 · openapi 7176 · wordpress 6668 · axios 2646 · graphql 2246
angular 2170 · grape 1948 · chi 1837 · nestjs 1571 · laravel 1533 · aspnetcore 1192
nextjs 1009 · fetch 1007 · play 923 · request-options 803 · express 651 · spring 578
symfony 574 · fastapi 432 · resttemplate 423 · gin 422 · scalatra 350 · flask 326
dart 173 · go_router 156 · grpc 134 · vue 133 · axum 106 · auto_route 100
utoipa 87 · graphql-ruby 81 · net/http 62 · nuxt 55 · blazor 48 · openapi-fetch 46
guzzle 44 · navigator 43 · http-client 42 · httpclient 36 · net-http 34
pekko-http 29 · faraday 25 · sveltekit 24 · requests 21 · django 21 · http4s 21
graphql-sdl 18 · file-get-contents 12 · client-seam 11 · httpx 10 · hono 10
gorilla/mux 5 · aiohttp 5 · retrofit 5 · urlsession 5 · razorpages 2
```

**`openapi 7176` against 1,998 at v465 is the new corpus, not a new reader.** medusa
publishes its whole API as OpenAPI specifications (4,792 operations), nhost 250 and
directus 136. The same seven repositories account for the rise in `fetch`, `express`,
`nextjs`, `axios` and `gin`, and `graphql-sdl 18` is the schema fields of nhost and
cypress-realworld-app. Outside them the counts moved by 34: a server route mounted at `/`
used to be dropped as an empty path, and peertube's Express server gains 29 of them.

**`laravel 1533` joined at v465.** The four Laravel applications joined the corpus for that
run, and the reader that first met them found no routes in one and almost no handlers
in another: see [the Laravel rows](#php--laravel) for what that turned up.

**`angular 2170` is the sixth-largest framework and was zero.** It counts route facts
only — 612 page routes composed across files plus the client calls a component makes
— which is a small fraction of what the dialect reads: 12,174 classes carrying a
container role, 36,338 template references, 12,754 composition edges and 7,975
injection edges do not appear in a route tally at all. `axios` and `nestjs` grew for
the same reason the Angular rows exist: the corpus gained ten repositories, three of
which carry a TypeScript backend beside the Angular client.

**rails fell from 10,776 to 7,963, and the drop is the point.** The reader now honours
what a Rails route table actually declares: `resources :profiles, only: :show` is one
route, not seven, and a `scope constraints: { format: :html }` block is not a `/html`
path prefix. On rubygems.org that is 569 route facts down to 246 — the removed ones
include a `DELETE /api/v1/api_key` against a resource declared `only: %i[show create
update]`, and 200-odd paths that were reported one segment deeper than they are served.
A route count is not a score: an extractor that invents six verbs per resource scores
higher and is wrong more often.

The earlier growth still stands underneath it — routes used to be collected from the
root `config/routes.rb` alone, so every engine and plugin route file went unread, and
Grape had no extractor at all, leaving GitLab's entire v4 REST API invisible behind a
single `mount ::API::API` (`grape 1,948`).

Fact kinds: 6,524,349 symbols · 2,290,316 dependencies · 139,054 file refs ·
106,040 test refs · 91,314 modules · 46,058 routes · 7,150 associations ·
5,893 storage · 248 extraction · 18 intent. Tailwind design tokens are symbols, and
in-process event topics are storage, so neither is a kind of its own. **`association` is new in 0.4.0** — a
model's declared `has_many`/`belongs_to` relations, which is what lets `endpoint`
walk from a URL to the tables behind it. Service nodes
are absent here by construction: the sweep indexes one repository at a time, and a
service node only exists in a multi-repo graph — those are section 3's subject.

**Scala's three route surfaces appear at `play 923`, `pekko-http 29` and
`http4s 21`**, and the shape of that split is the point rather than the totals. Play
declares a whole application's endpoints in one file, so one application supplies
923 of them; the other two are DSLs written per route tree, and the corpus holds one
real application for each beside the library that defines it. A library defines a
DSL and does not use one — which is why counting `pathPrefix` in a routing library
finds the directive's own definition and almost no endpoints.

`aspnetcore` at 1,192 is among the dozen largest frameworks, and `storage` is non-zero for
.NET at all — both were **zero** before this corpus was indexed for it. `blazor 48`
and `razorpages 2` count `@page` routes only; the bulk of Razor's contribution is
reference edges rather than routes, which is what the format is mostly used for.

Two rows are worth reading carefully rather than as a score. **django 21** counts
routes, not coverage: 17 are the Django repository's own, and 4 are Saleor's. Saleor is
GraphQL-first, so its REST surface really is that small, while its facts still carry
the Django tag too. And **`utoipa 87` is a row that did not exist one
sweep ago**. Two Rust services in the corpus declare part of their API through
`utoipa_axum`'s `routes!()` macro, which registers a handler without repeating its
path: the path is written in a `#[utoipa::path]` attribute on the handler. Reading
that attribute takes **crates.io from 8 served routes to 74** — 57 distinct paths,
against the 91 client calls its own frontend makes — and cognee-rs from 77 to 98. The
macro is still not expanded; it does not have to be. See [rust.md](extraction/rust.md).

What that surfaced is worth more than the count. On crates.io the `domain` explainer
was reporting **19 components as calling outbound endpoints** — an inventory of
third-party integrations that do not exist, because those endpoints are served by the
Rust half of the same repository and enola could not see the routes. The explainer
checks for exactly this and excludes a call the graph shows as self-served; it had
nothing to check against. With the routes visible all 19 are gone, and on a cluster
containing crates.io the calls resolve: 2 of 91 call sites resolved before, 90 of 91
after. A missing extractor does not only subtract facts — it manufactures findings,
and they read exactly like real ones.

A third belongs to .NET. OrchardCore contributes 82 routes where it declares 288
verb attributes, because 94% of its controllers are conventionally routed and 20 of
its registrations use a `{area}/{controller}/{action}` template that cannot be
resolved without each controller's area. That gap is counted and logged rather than
closed by guessing — see [dotnet.md](extraction/dotnet.md).

## 6. Agent A/B — does the agent ship the regression?

> Unlike sections 1–5, this one was **not** re-run for the current corpus: it needs
> real agent sessions rather than a script, so its numbers are carried forward from
> the earlier run. The fixture and the finding are unchanged; only the date differs.

This is the regression gate end to end, on a fixture built to trip it. Whether enola
changes an agent's answers on a real system is the
[agent A/B on code the model has not seen](#agent-ab-on-code-the-model-has-not-seen).

The one A/B worth running here is against **no tool**, on a task where the obvious
implementation is structurally wrong.

**Setup.** A small TypeScript service with three modules: `src/domain`, `src/store`
(imports domain), `src/api`. The task: *add `recentOrders(customerId)` to the domain
package and expose it from the API handler.* The natural implementation makes
`domain` import `store`, closing a module cycle. TypeScript compiles it without
complaint and every test still passes.

Three arms, three trials each, Claude Code headless (`claude -p`, Claude Code 2.1.205).
Each arm adds one thing to the one before it:

- **bare** — no MCP servers, no instruction, no hooks.
- **instruction** — enola's MCP server plus the exact instruction `enola install`
  writes into the project's agent rules, which tells the agent to pin a baseline
  before editing and diff afterwards.
- **loop** — the same, plus the session hooks `enola install --hooks` configures, so
  the change is graded when the agent stops whether or not it remembered to ask.

Ground truth is identical for all three and does not trust the agent: the tree each
run leaves behind is graded against a baseline pinned before it started.

| Arm | Shipped the regression | Median turns | Median cost | Median wall |
|---|---|---|---|---|
| bare | **3 / 3** | 12 | $0.31 | 53s |
| instruction | **1 / 3** | 14 | $0.66 | 125s |
| loop | **0 / 3** | 24 | $1.12 | 252s |

The bare agent shipped a dependency cycle every time. With the hooks installed, none
of the three did, and the transcripts say why. From one of them, unprompted:

> The regression was `src/domain ↔ src/store`: my first attempt made the domain
> `import { listOrders }` from the store, which closed a loop with the store's
> pre-existing `import type { Order }` from the domain. I removed the offending edge.

The loop worked as designed: the agent introduced the cycle, the Stop hook graded
the session and handed back the verdict, and the agent corrected itself before
reporting done. It took roughly double the turns and 3.6× the money of the bare
run: the cost of catching and fixing the regression, against the bare run's
$0.31, which bought a tree with a dependency cycle already in it.

**Read this before quoting the table.**

- **Nine runs is not a statistic.** 3/3 versus 0/3 is a real gap and the ordering is
  monotonic, but the difference between the instruction arm's 1/3 and the loop arm's
  0/3 is one run. Treat this as a demonstration that the mechanism works end to end,
  not as a measured effect size.
- **No arm ever called `diff_snapshot`.** Every enola-equipped run used exactly one
  tool, `generate_snapshot`. The loop arm did not succeed because the agent asked
  enola the right question. It succeeded because something asked on its behalf when
  it stopped. That is the whole argument for the hook over the instruction.
- **The cost column is not enola's price.** It compares an agent that checked its work
  against one that did not: the bare arm is cheap because it stopped early and shipped
  the cycle three times out of three. The arm that would isolate what enola costs does
  not exist here: an agent told to establish the same property *without* a graph,
  re-deriving the module structure from source on every task. Do not read $0.31 → $1.12
  as the overhead of adding enola to a run that was otherwise identical.
- **The instruction arm improved on bare anyway** (1/3 vs 3/3), which is worth being
  honest about: having a structural map in context appears to help even when the
  agent never queries the delta. With three trials, that could equally be noise.
- **One trap, one language, one model.** A deliberately subtle module-level cycle in
  TypeScript.

## 7. Framework questions: are the answers right?

Sections 1 to 5 grade the graph as a whole. Graph size and fact counts can grow while
the answer to a specific question gets worse, so this section grades answers. Six
questions that a framework makes hard, each against a key read in source:

| | The question | Framework |
|---|---|---|
| **Q1** | Which resolver implements this field, and what depends on it? | GraphQL (SDL and code-first) |
| **Q2** | Which components does a change to this composable or store reach? | Vue, Nuxt, Pinia |
| **Q3** | Which tests exercise this route, operation or component? | Cypress, Playwright |
| **Q4** | Does an import through an alias reach the right module, and by what? | Vite, Nuxt, tsconfig `paths` |
| **Q5** | Which components use this design token? | Tailwind (v3 config, presets, v4 `@theme`) |
| **Q6** | What publishes and what subscribes to this event or job queue? | Node `EventEmitter`, Medusa, Directus, BullMQ |

Seven repositories carry the key: directus, elk, cypress-realworld-app, nhost, medusa,
vite (its alias playground) and vite-tailwind-graphql. tailwindcss is in the sweep as
the control for Q5: the library that defines the utility classes must not read as their
user. elk is a second control: it styles with UnoCSS, so it must yield no token at all.

The key is 88 rows, each with its `file:line` evidence. A row is a path that must exist,
a path that must not, a fact, a dependency that must resolve by a stated mechanism, an
impact set, or an **unresolved** fact that must say why. Unresolved with the right
reason scores as correct: a Hasura GraphQL operation has no resolver in the repository,
and the honest answer names that rather than guessing one.

| | found | unresolved, reason right | negatives held | claims | correct |
|---|---:|---:|---:|---:|---:|
| Q1 GraphQL | 8 / 8 | 2 / 2 | 1 / 1 | 18 | 100% |
| Q2 Vue | 22 / 22 | | 2 / 2 | 25 | 100% |
| Q3 tests | 12 / 13 | | 2 / 2 | 120 | 100% |
| Q4 aliases | 12 / 12 | 1 / 1 | | 12,175 | 1 wrong |
| Q5 Tailwind | 10 / 10 | | 2 / 2 | 8,581 | 100% |
| Q6 events | 11 / 11 | 1 / 1 | 1 / 1 | 206 | 100% |

Before this work, at v465, the same key read Q1 0/8, Q2 14/22, Q3 0/13, Q4 1/12, Q5 0/10
and Q6 0/11. For four of the six questions the node a question starts from did not
exist. Across the six, **75 of 76 rows** are found now, against 15 of 76 then.

**Precision counts every claim, not a sample.** Q1 to Q3 make tens to a hundred claims
per repository, and each was read in source. Q4 and Q5 make thousands, so each is
checked against the file it names: an alias target must be a file or folder module of
the repository, a token use must be a class in that file built from the section's
utility prefix and the token. A sample of each was read by hand as well. The one wrong
claim is a tsconfig `paths` entry with two targets, where enola takes the first and
the file is the second.

**The miss is stated.** Q3's thirteenth row is a `cy.visit` into a page that React Router
declares in JSX. `<Route>` elements produce no page facts, so the test reaches no route.

**Impact under a controlled change.** One change per question is applied to a scratch
copy: rename a GraphQL field, move a composable, remove a route, change an alias, rename
a token, rename an event. `impact_analysis` on the target before the change is graded
against the dependents the key lists, then `diff_snapshot` must name the change.

| change | impact names the dependents | diff names the change |
|---|---:|---:|
| rename a GraphQL field | 100% | 1 / 1 |
| move a composable | 100% | 1 / 1 |
| remove a route | 100% | 1 / 1 |
| change an alias | **0%** | 1 / 1 |
| rename a token | 100% | 1 / 1 |
| rename an event | 100% | 2 / 2 |

The alias row is the open one. Its only dependent is an HTML entry, which has imports
but no symbol for impact to walk from.

All seven repositories produce a byte-identical `facts.jsonl` across two snapshots. The
harness is `bench-semantics.sh` in the benchmarks repository, and `bench-all.sh` fails
when a row the pin found is lost.

## What this does not measure

Stated so the numbers above are not read as more than they are.

- **A task-level A/B beyond one system.** The
  [agent A/B](#agent-ab-on-code-the-model-has-not-seen) runs the same tasks with and
  without enola on one system of five repositories, with one model, and the agent told
  to use enola. The real-session measurement runs nothing twice. Section 6's cost column
  is a third figure: it compares checking against not checking, not one path to an
  answer against another.
- **The session hooks in interactive use.** Section 6 exercises them headlessly, under
  `claude -p`. The configuration is the same either way, but interactive sessions were
  not measured.
- **Swift in the ratchet.** Measured in every other section; excluded from section 2
  only, because a Swift module is a declared SPM target and no additive cycle
  injection exists. Stated above rather than silently omitted.
- **Scala has no cross-repo cluster.** It is measured in sections 1 and 2 across
  nine repositories, but section 3 pairs a backend with its own clients and no
  Scala pair exists in the corpus, so Scala contributes nothing to the coverage
  numbers. Its client-side extraction (sttp, Play WS) is exercised by unit tests
  and the golden fixture only.
- **Kotlin and Swift at *large* scale.** Both now have a real public repository
  (Google's reference Android app; a SwiftUI application), but at 312 and 382 parsed
  files they are the small end of the corpus. The framework constructs are exercised
  on production code; the scale claims are not.
- **Vue and Svelte route surfaces are small.** Nuxt and SvelteKit are measured on real
  applications, but 56 and 13 routes respectively, enough to show file-based routing
  works, not enough to characterise it.
- **Recall of cross-repo edges.** Section 3 reports what enola detected and what it
  resolved. It does not report edges enola never detected at all: that would need a
  hand-labelled ground truth for each cluster, which does not exist here.
- **Framework recall beyond the key.** Section 7 grades every claim enola makes in its
  six areas, so its precision is measured. Its coverage is 88 relationships on seven
  repositories, read by hand; a relationship of the same kind elsewhere that enola never
  claims is not counted.
- **A ranking against other tools.** The harness has adapters for graphify, codegraph
  and codebase-memory-mcp, and they ran on the same questions, each measured against
  its own claims beside the same bare arm. Nothing on this page ranks enola against
  them.

## Reproducing

```bash
# one repository, three times — the reproducibility check in miniature
enola --generate /path/to/repo && cp .enola/receipt.json /tmp/r1.json
enola --generate /path/to/repo && cp .enola/receipt.json /tmp/r2.json
diff <(jq -r .snapshot_id /tmp/r1.json) <(jq -r .snapshot_id /tmp/r2.json) && echo reproducible

# the ratchet, on your own repository
enola baseline pin .          # freeze the architecture
#   …make a change…
enola check --fail-on=cycles .   # exit 1 on a NEW cycle; a bare `check` reports
                                 # everything and exits 0, so name what should fail

# cross-repo coverage, misses included
enola coverage cluster.yaml
```

The agent A/B runs on [graph-bench](https://github.com/dejo1307/graph-bench): the same
question through headless Claude Code, bare and with a tool installed the way its docs
say, scored against a key you build from your own code. It holds the method and the
tooling; your questions, keys and results stay with you.

> **Read the `enola: using config …` line each command prints, and check it is the
> config you meant.** A config's `extractors:` list *replaces* the built-in default
> rather than merging with it, so the wrong one does not fail — it quietly analyses
> something other than what you asked for. enola looks in the working directory first
> and falls back to the directory holding the binary.

---

Per-language extraction detail: **[docs/extraction/](extraction/README.md)** ·
What the words mean: **[docs/GLOSSARY.md](GLOSSARY.md)** ·
Commands and flags: **[docs/CLI.md](CLI.md)** ·
Why the graph is a snapshot: **[docs/SNAPSHOTS.md](SNAPSHOTS.md)** ·
What the findings are for: **[docs/EXPLAINERS.md](EXPLAINERS.md)** ·
How the engine works: **[ARCHITECTURE.md](../ARCHITECTURE.md)**
