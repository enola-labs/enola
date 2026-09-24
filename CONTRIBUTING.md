# Contributing to enola

Thank you for your interest in contributing to enola. Every contribution — code, documentation, bug reports, feature ideas — helps make the project better for everyone.

## Getting started

1. **Fork and clone** the repository.
2. Make sure you have **Go 1.26+** and a **C compiler** (for tree-sitter bindings).
3. Build and verify:

   ```bash
   go build -o enola ./cmd/enola
   go test ./...
   ```

4. Create a branch for your work:

   ```bash
   git checkout -b my-feature
   ```

5. **Enable the pre-push hook** — once per clone:

   ```bash
   git config core.hooksPath .githooks
   ```

   It runs the guards that are cheap locally and awkward in CI: the `cacheVersion`
   coverage check, and the golden + determinism suite. Skip in an emergency with
   `git push --no-verify`; CI enforces both anyway.

   It also runs one check **CI cannot run at all**. See below.

## The architecture gate on your pull request

CI runs enola on enola: your change is graded against the layer order this repository
declares in [`enola-intent.yaml`](enola-intent.yaml), with `--fail-on=layers`. A package
depending outwards — anything under `internal/` reaching up into `pkg/command`, say —
fails the job and the finding names the import that did it.

Two things follow for a contributor:

- **Moving or adding a package means updating the declaration.** A package no declared
  path matches is not a violation, it is *unclassified*: it produces no findings, so the
  gate would quietly stop covering your code. `go test ./internal/intent/` fails when
  that happens, which is the only reason it is caught.
- **Nothing else is enforced.** enola fails on what a policy names and nothing more, so
  cycles, god-classes and the rest are reported on your pull request and allowed
  through. That is deliberate — see [docs/GATING.md](docs/GATING.md#what-fails-the-build).

## Measuring memory

Two hidden flags, kept out of `--help` because they are development instruments
and cost a stop-the-world heap read every few milliseconds:

```bash
enola --memstats --generate /path/to/repo             # one summary line on stderr
enola --memprofile heap.pb.gz --generate /path/to/repo # plus two heap profiles
```

`--memstats` prints peak and steady heap, `Sys`, total allocation, mallocs and the
fact count. **Peak is the figure that matters and it is not the one the engine's own
end-of-run log line reports** — that runs after `FreeOSMemory` and describes the
survivor. On a large repository the two differ by five to eight times.

`--memprofile` additionally writes a heap profile at the peak (to the given path,
rewritten at each new high-water mark) and one of the steady state (`.final`):

```bash
go tool pprof -alloc_space -top  enola heap.pb.gz        # what churned
go tool pprof -inuse_space -top  enola heap.pb.gz        # what was live at the peak
go tool pprof -inuse_space -top  enola heap.pb.gz.final  # what a loaded graph costs
```

Do not compare these against `ps rss` or macOS "footprint": Darwin keeps freed pages
resident, so both read several times the live heap and do not move when memory is
genuinely returned.

`enola-benchmarks/bench-sweep.sh` records these per repo and
`bench-mem-ratchet.sh` grades them against pinned ceilings; the plan they serve is
`enola-benchmarks/MEMORY_IMPROVEMENTS.md`.

## What to work on

- **Bug reports and fixes** — if something doesn't work, open an issue or submit a fix.
- **New language extractors** — enola's architecture makes it straightforward to add support for additional languages. See [`ARCHITECTURE.md`](ARCHITECTURE.md) for how extractors, explainers, and renderers fit together.
- **Improved detection and extraction** — better framework support, more accurate dependency resolution, richer symbol extraction.
- **Documentation** — typo fixes, clearer explanations, new examples.

If you're considering a larger change, please open an issue first so we can discuss the approach before you invest significant time.

## Submitting changes

1. Keep commits focused. One logical change per commit.
2. Write clear commit messages that explain *why*, not just *what*.
3. Add or update tests for any code changes.
4. Make sure all tests pass before submitting:

   ```bash
   go test ./...
   ```

5. Open a pull request against `main`. Describe what the change does and why.

### If you touch the agent hooks

`pkg/install/`, `pkg/command/hook.go`, `pkg/command/doctor.go` and `internal/hookstate/`
are covered by one test CI will never run for you:

```bash
ENOLA_E2E=1 go test -run TestStopHook_FiresInARealSession ./pkg/install/
```

It installs the hooks the way a user does, ends a **real agent session** with a known
regression present, and asserts the verdict came out. CI runners do not run agent
sessions by design, so this only ever executes on a developer's machine — the pre-push
hook runs it automatically when your push touches one of those paths, and skips with a
loud message if `claude` is not on your `PATH`.

Please do not treat it as optional. The failure mode it guards against is a hook
configuration that parses, reports success, and does nothing — and every cheaper check
passes while that is true, because a unit test can only compare the output against the
same belief that produced it.

If you cannot run it, say so in the PR so a reviewer can. `enola doctor` is the
same question asked after the fact, on a real repository.

## Adding a language, a connection, or an analysis

enola's middle is plugins, and which kind you need is worth two minutes before you start:

| You want to… | See |
|---|---|
| Parse a language or framework enola cannot read | [docs/extraction/README.md](docs/extraction/README.md) |
| Connect facts within one repo that no single extractor could see (a binder) | [docs/EXTENDING.md](docs/EXTENDING.md#adding-a-binder) |
| Establish that one repo depends on another (a cross-repo signal) | [docs/EXTENDING.md](docs/EXTENDING.md#adding-a-cross-repo-signal) |
| Stop a wrong edge, or teach enola a framework's boilerplate | [docs/EXTENDING.md](docs/EXTENDING.md#tuning-without-code) — usually config, not code |

One rule governs all of it: **a missing edge beats a wrong one.** A missing edge shows up
in `enola coverage` as an unresolved count somebody can go and look at; a wrong edge is
invisible and gets acted on. If you are unsure whether to draw one, don't.

Two mechanical things that are easy to miss: an **extractor** change needs a `cacheVersion`
bump in `internal/engine/cache.go` plus an entry in `internal/cachecov` (binders, signals
and linkers sit outside that cache and need neither), and any guard you add should be made
to fail once before you trust it — a test that has never failed is not a guard.

## Code style

- Follow standard Go conventions (`gofmt`, `go vet`).
- Prefer clear code over clever code.
- Default to no comments — add one only when the *why* is non-obvious.

## Reporting bugs

Open a [GitHub issue](https://github.com/enola-labs/enola/issues) with:

- What you did (command, config, repository structure).
- What you expected.
- What happened instead.
- enola version and OS.

## Community standards

All participants are expected to follow our [Code of Conduct](CODE_OF_CONDUCT.md). We are committed to making this a welcoming and respectful community for everyone.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
