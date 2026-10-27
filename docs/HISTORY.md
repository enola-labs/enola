# The architecture history

*How enola remembers what your architecture used to look like, and why it is allowed to.*

`enola log` answers a question a snapshot structurally cannot: **when did this happen?**

> When did `internal/server` start importing `internal/extractors`?
> Which change introduced this dependency cycle?
> What did this codebase look like in March?

Every other surface in enola describes the tree as it is now — `diff_snapshot` included, which
compares two nows. This one describes the past.

**Experimental.** The commands and the on-disk format may change.

---

## Reading a history

```
$ enola log
e8847af  2026-08-03 17:54  (d2293a18, main)   +1 facts · ~1 changed · +6 edges
0bd6c73  2026-08-03 17:55  (d2293a18, main)   +3 facts · +12 edges
c1caa0a  2026-08-03 17:57  (8db49777, main)   no architectural change
3cdf107  2026-08-03 18:07  (8db49777, main)   1 new finding · +36 facts · +127/-1 edges
```

Oldest first — the opposite of `git log`. A changelog answers *what landed recently*; this answers
*how did it get like this*, and that question runs forward. With `--graph` the lines therefore
diverge downward at a branch and converge downward at a merge.

| Command | Question |
|---|---|
| `enola log` | what has this architecture done over time? |
| `enola log --graph` | …and along which branches? |
| `enola log --stat` | …broken down by what kind of thing changed |
| `enola show <rev>` | what did THIS revision do? |
| `enola diff <a>..<b>` | what happened between these two points? |
| `enola blame <pattern>` | when did this appear, and when did it go? |
| `enola gc` | what is stored, and what can go? |

A revision is a snapshot id or its prefix, a git commit, `HEAD~3`, `@7`, a ref name, or `latest`.

Agents reach the same two questions through `architecture_history` and `architecture_blame`.

### blame

```
$ enola blame "internal/engine -> internal/history"
e8847af  2026-08-03 17:54  (d2293a18, main)
    + dependency  internal/engine -> …/internal/history  (internal/engine/history.go:11)
```

The pattern matches a module or symbol name, a file path, or both endpoints of an edge. `--findings`
searches recorded findings instead ("which snapshot introduced this cycle?"); `--first` stops at the
introduction.

A symbol that changed between two revisions is reported as one change, not as a removal and an
addition, and its body version (`body_hash`, see [the facts schema](schema/facts.md#body_hash)) says
which kind:

```
$ enola blame App.onPointerMoveFromPointerDownHandler
fefa0b2  2025-07-24 17:02
    ~ rewritten  symbol  …components.App.onPointerMoveFromPointerDownHandler  (App.tsx:8037)  was App.tsx:8041
b8924d7  2025-08-11 11:52
    ~ shifted    symbol  …components.App.onPointerMoveFromPointerDownHandler  (App.tsx:8038)  was App.tsx:8037
```

- **shifted**: same code, a new line, because something above it changed.
- **rewritten**: its code changed.
- **moved**: it left one file or package and arrived in another with the same body and the same
  name inside its module. Its id changed with the file; the pair is how you follow it.
- **changed**: one side has no body version (a revision recorded before versions existed, or a
  language with no spans), so a shift cannot be told from a rewrite.

On fifteen months of one repository's daily history this was the bulk of what blame used to print as removed
and re-added: 43,783 shifts against 3,412 rewrites and 138 moves.

Revisions whose stored contents have aged out are reported as **unsearched**, never as absent —
"not found" and "not found in what I could read" are different answers, and the second means look
further back.

---

## Starting one

Recording is **on by default**: every `generate_snapshot` appends a revision.

That default is deliberate and is the opposite of how a feature like this usually ships. A history
answers questions about the PAST, so opt-in guarantees that the first time anybody wants one there is
nothing to read. What makes it affordable is that a revision is a ~600-byte line, working-tree
snapshots are capped per commit, and the whole thing lives outside your repository.

To read a past that predates enola, build it:

```
$ enola log --backfill --sample=daily
165 commit(s) selected, 0 already recorded, 165 to snapshot.
Backfilled 165 revision(s) in 3m12s.
```

`--sample=all|merges|tags|daily`, `--since`, `-n` and `--dry-run` choose what to walk. It **reads**
your repository and writes nothing into it: each commit's tree is extracted to a temporary directory,
so a read-only or borrowed checkout is a valid target. Re-running resumes rather than repeating.

Two things to know about a backfilled revision. It is stamped with the **commit's** date, not the
moment you ran the command. And it is snapshotted with **today's** enola and today's config — not the
config that happened to be committed alongside that commit, which would make every historical config
edit look like an architectural event.

---

## Where it lives, and what it costs

`~/.enola/graphs/<workspace>/history/` — outside your repository, so `enola log` is safe to run
against a checkout you do not own, and clearing `.enola` (everything in there is derivable) does not
take the history with it. Set `history.dir` to move it, for instance to commit it or publish it from
CI.

```yaml
history:
  enabled: true      # record each snapshot as a revision
  blobs: true        # store each revision's graph, so it can be replayed
  blob_keep: 200     # roughly how many recent revisions keep their contents
  working_keep: 20   # working-tree revisions kept per commit
  dir: ""            # override the location
```

Measured:

| | header | with contents |
|---|---:|---:|
| ~4k-fact repository | ~600 B / revision | ~4 KB / revision |
| ~23k-fact repository | ~600 B / revision | ~34 KB / revision |

165 revisions of a substantial frontend cost 5.6 MB. Storage is linear in graph size, not in history
length: a revision is stored as a patch against the previous one, with a full copy every so often.

`enola gc` reports what is there and removes what is not needed. With no flags it removes only
garbage; `--thin-older-than=90d` drops old contents while keeping the timeline, and
`--prune-working` discards uncommitted-tree snapshots.

---

## Sharing a history across machines

Everything above is per-machine: blame and diff can only answer about what *this* machine saw.
`enola history` shares the record through a **directory store** — plain files that can live
anywhere files can: a git repository, a shared mount, an S3-synced folder. No daemon, no
database, no network code in enola itself; whatever already syncs the directory is the transport.

```
$ enola history push /mnt/arch-history      # copy local revisions in
$ enola history pull /mnt/arch-history      # import what other machines pushed
$ enola history verify /mnt/arch-history    # walk every chain; name gaps and tampering
$ enola history gc /mnt/arch-history --keep-last 500          # prints; deletes nothing
$ enola history gc /mnt/arch-history --keep-last 500 --apply  # prunes, on record
```

Set `history.shared_dir` in the config and the store argument can be dropped — and `blame` and
`diff` then answer from the **union** of the local history and the store, naming where every
revision came from:

```
$ enola blame Billing::Ledger
e8847af  2026-08-03 17:54  (d2293a18, main)  [local, store:dev-laptop-1f2a]
    + symbol      Billing::Ledger  (app/models/billing/ledger.rb:4)
$ enola diff aaaa000..bbbb111
# aaaa000..bbbb111 · 2026-03-02 → 2026-08-03 · left from store:ci-runner-3c9d, right from local
```

### The store contract

```
<store>/format                      the format marker ("shared-history/1")
<store>/entries/sha256-<id>.json.gz one immutable file per revision, named by snapshot id
<store>/chains/<source>.jsonl       one append-only chain per pushing machine
```

An **entry** carries a revision's canonical fact and insight lines plus the provenance a diff
needs (versions, plugin sets, config hash) — and nothing observation-specific: no timestamp, no
sequence number, no branch name. That is what makes it content-addressed in the strong sense:
the same snapshot pushed from two machines produces **byte-identical** files, which is why a
sync-level last-writer-wins on an entry file can never lose information.

A **chain record** is the observation: who saw that snapshot, when, at which commit. Each record
carries the SHA-256 of its predecessor's line, so the chain is tamper-evident — edit, remove or
reorder a record and every record after it stops verifying. Each machine also remembers the last
record it pushed, and refuses to push onto a store whose chain no longer contains it, which is
what catches a chain truncated from the end.

### Why concurrent writers cannot corrupt it

The store is safe under any file-level sync because no file ever has two writers:

- **Entry files are immutable and content-addressed.** Two machines pushing the same snapshot
  write the same bytes under the same name; a conflict resolved either way is a no-op. A pusher
  that finds the file present writes nothing.
- **Each chain file has exactly one writer** — the machine named in its filename. Two machines
  pushing concurrently touch two different chain files, so there is nothing for a syncer to
  merge and no interleaving to get wrong.

The price of that shape is that ordering across machines is reconstructed at read time (by
timestamp, exactly as a backfilled local history already is), not recorded — a price the local
format already decided to pay when it made `Seq` machine-local.

### Retention that stays honest

`history gc` takes `--keep-last N` and/or `--keep-since` (revisions satisfying either are kept),
and never deletes silently: without `--apply` it only prints what would go. An applied prune
appends a **prune record** to the chain naming every removed revision, so forever after, a
missing payload divides into two different answers — *pruned by retention on this date under
this policy* (verify counts it, pull and blame report it as pruned) and *a gap the store cannot
explain* (verify names it as a problem). Chain records themselves are never deleted: the
timeline of what was observed outlives the contents.

### Setting one up for an org

1. Pick a location every machine can sync: a dedicated git repository, an NFS mount, an
   S3/GCS bucket behind `rclone`/`aws s3 sync`.
2. Set `history.shared_dir` in the repository's `mcp-arch.yaml` (or pass the directory
   explicitly), and `enola history push` after snapshots — from CI, a cron entry, or by hand.
3. New machines run `enola --generate` once (so the repository's identity is recorded), then
   `enola history pull`. From then on `blame` and `diff` answer over the shared record.
4. `enola history verify` in CI is the cheap standing check that the store is intact.

A revision pulled from another machine keeps that machine as its `origin` — `log`, `blame` and
`show` print it — so a historical claim is always attributable to the machine that observed it.

---

## The rule this feature lives under

[SNAPSHOTS.md](SNAPSHOTS.md) rests on a claim about **authority**: everything enola writes is
derivable from your tree, and nothing it keeps can tell you something your source does not say. A
history is the first thing enola keeps that the working tree has forgotten, so it is bounded by three
properties, each enforced by a test rather than by intention:

- **It is replayable.** Every revision is reproducible by re-running enola at that commit; that is
  what `--backfill` does. Nothing here is a measurement that could not be taken again.
- **Nothing that judges the present reads it.** `check`, `diff_snapshot`, freshness and drift consult
  only the current snapshot and the pinned baseline. Deleting the history changes no verdict, and no
  `snapshot_id`.
- **Deleting it loses convenience, never truth.**

The third point is why `previous/` and `baseline/` are still full copies under `.enola/` rather than
references into the history. Making them references would save a few megabytes and would mean that
removing the history could change what `enola check` says — which is exactly the dependency the rule
forbids.

### When enola itself changes

A delta is only a description of somebody's change when both sides were produced the same way. Bump
an extractor, enable a language, edit an ignore glob, and hundreds of facts move because the PARSER
changed. Each revision records an **epoch** — a fingerprint of enola's version, its extraction
behaviour, the effective config and the plugin sets — and the log marks the seam:

```
* 9d930a7  2026-08-03 18:37  (25f265c8, main)  no architectural change
| ══ epoch changed (afb99d → 846a1f) — the delta below is rebuild noise, not a change to the code
* 5a6d794  2026-08-03 18:43  (25f265c8, main)  ~1 changed · +1 edges
```

Revisions marked `incomparable` sit across such a seam. Elapsed time alone never earns that mark:
revisions months apart are the normal shape of a timeline, not a defect.

---

## What it cannot tell you

- **Only what was observed.** enola sees the commits somebody snapshotted, which is a sparse subset
  of your history. `--graph` closes the gaps by asking git which observed revision is the nearest
  ancestor of which — the same parent rewriting `git log --graph -- <path>` does — so the picture is
  honest about being sparse rather than pretending to be complete.
- **A summary is measured against the PREVIOUS SNAPSHOT, not the graph parent.** Usually the same
  revision. Switch branches between snapshots and they differ: the row reports what changed since
  enola last looked, which can include another branch's work arriving or leaving. `--graph` says so
  when it happens, and `enola diff <a>..<b>` answers the ancestry question directly.
- **A multi-repo graph has no single commit**, it has one per repository. The shape follows the
  primary repository and each row carries the others' positions; no DAG is invented over a vector.
- **A backfilled revision only sees what git tracks.** Generated or untracked files present in a
  working tree are absent from an extracted one.
