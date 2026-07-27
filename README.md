# recall

You solved this before. Six weeks ago, in a session you can't find.

Claude Code and Codex write every session to disk as JSONL and then give you no
way to search it. `recall` indexes both tools into one local SQLite store and answers
natural-language or keyword queries from the terminal — hybrid **semantic + full-text**
ranking, single Go binary, no cloud service in the retrieval path.

```sh
$ recall "how did I fix the pyexpat linking" --pretty
DATETIME          SOURCE  SESSION   PROJECT     TITLE
2026-04-17 22:31  claude  4a4b4bfe  build-tools  Static-linking pyexpat into the wheel
```

It also doubles as a session manager: list, pin, summarize, and resume past sessions
(see [Command reference](#command-reference)).

> **Privacy, stated precisely:** indexing and search are local — but `recall summary`
> sends transcript text to the `codex` CLI, and a remote `OLLAMA_HOST` sends text
> off-box. Read [Privacy & network behavior](#privacy--network-behavior) before you
> point this at sensitive transcripts.

## Documentation map

| Doc | What it covers |
|---|---|
| [`DESIGN.md`](./DESIGN.md) | Architecture, schema, every design decision + the alternatives rejected (why SQLite over LanceDB/DuckDB/turbopuffer, why Go, why brute-force vectors). |
| [`docs/embedding-models.md`](./docs/embedding-models.md) | Which embedding model recall uses and **why** — the bge-m3 vs qwen3 comparison, results, and the decision. |
| [`docs/evaluating-embeddings-101.md`](./docs/evaluating-embeddings-101.md) | **Beginner's guide** — what embeddings are, how search is benchmarked, what Recall@k / MRR mean. Read this if the comparison doc feels jargon-heavy. |
| [`eval/`](./eval/) | The benchmark that picked the default model: query set (`fixture.json`), harness (`run.py`), results (`results.json`). **Not reproducible by outsiders** — see [Evaluation](#evaluation-and-why-you-cant-reproduce-it). |

## Prerequisites

- **Go** — only if you build from source. `go.mod` declares **1.26.3**, so that's the
  minimum the toolchain will accept. The result is a single binary (no cgo). Not needed
  if you install with Homebrew.
- **[Ollama](https://ollama.com)**, running, with an embedding model pulled:
  ```sh
  ollama pull qwen3-embedding:0.6b   # 639 MB
  ```
  Required for `recall index` and for semantic/hybrid search. Lexical search and
  `--all` need no model.
- **[`codex`](https://github.com/openai/codex) CLI** — *only* for `recall summary`, which
  shells out to it. Must be installed **and authenticated**. Every other command works
  without it.

Developed and used on macOS (arm64). Nothing is knowingly platform-specific except the
transcript locations and the resume commands, but Linux/Windows are untested.

## Install

**Homebrew** (easiest — prebuilt binary, no Go toolchain needed):

```sh
brew install ramsrib/tap/recall
```

**`go install`** (installs the binary as `recall-cli`; rename or alias it to `recall`):

```sh
go install github.com/ramsrib/recall-cli@latest
```

**From source:**

```sh
git clone https://github.com/ramsrib/recall-cli
cd recall-cli
make install        # builds and installs recall → ~/.local/bin
```

`make install` warns if `~/.local/bin` isn't on your `PATH` and prints how to add it.
Install elsewhere with `make install BINDIR=/usr/local/bin`.

```sh
make            # list all targets
make build      # build ./recall in the repo dir only
make uninstall  # remove the installed binary
make clean      # remove the local build artifact
```

Or plain Go: `go build -o recall .`

## Quickstart

```sh
ollama serve &                 # if it isn't already running
ollama pull qwen3-embedding:0.6b

recall index                   # first run embeds everything — minutes, not seconds
recall "that vector db tradeoff discussion" --pretty
```

`recall index` is incremental: re-run it anytime and it only touches sessions whose
files changed. The first run is the slow one — it embeds your entire history.

## Command reference

Every command prints **compact single-line JSON to stdout by default** (agent-first: no
flag needed when a script or agent calls it). `--pretty` switches to a human-readable
table. Progress, warnings, and notes go to **stderr**, so stdout stays clean JSON.

### `recall index [--model NAME]`

Reconciles the index with disk: new and changed session files are parsed, chunked
(~512 tokens), embedded via Ollama, and written to `~/.config/recall/index.db`. Files
deleted from disk are dropped from the index.

- `--model NAME` — embed with a specific Ollama model. Re-embeds **only** the files not
  already on that model, so switching models is incremental and resumable, not one big
  reindex. Default: `$RECALL_EMBED_MODEL`, else `qwen3-embedding:0.6b`.
- Requires Ollama to be running with the model pulled.
- Also refreshes the sessions registry that `recall list` reads.

### `recall "QUERY" [flags]`

The default command — anything that isn't a known subcommand is treated as a query.
Results are collapsed to **one row per session** (best-scoring chunk wins).

| Flag | Default | Meaning |
|---|---|---|
| `--mode hybrid\|semantic\|lexical` | `hybrid` | `semantic` = embeddings only; `lexical` = FTS5/BM25 keywords only; `hybrid` = both, merged with Reciprocal Rank Fusion. |
| `--all` | off | Session-level AND: which session mentions *every* one of these words *somewhere* (even in different messages)? Pure lexical, no embeddings. |
| `--source claude\|codex` | both | Restrict to one tool. |
| `--project SUBSTR` | — | Substring match on the session's working directory. |
| `--since YYYY-MM-DD` | — | Only sessions on/after this date. |
| `--limit N` | `20` | Max results. |
| `--model NAME` | the index's current model | Rank against another model's vectors (they must already be indexed under it). |
| `--include-exec` | off | Include non-interactive `codex exec` / headless `claude -p` runs, which are hidden by default. |
| `--pretty` | off | Human table (`DATETIME · SOURCE · SESSION · PROJECT · TITLE`) instead of JSON. |

Flags may appear **before or after** the query. JSON fields: `source, session_id,
project, title, role, ts, path, score, snippet` — `path` is the transcript on disk, so
an agent can open the matched session.

```sh
recall "how did I fix the pyexpat linking"
recall --mode semantic "that vector db tradeoff discussion"
recall --mode lexical  "pyexpat"
recall "webhook retry backoff timeout" --all
recall "deploy script" --project backend --since 2026-05-01 --limit 10 --pretty
```

**`--all` vs the modes.** The modes match *within a single chunk* (one message span) and
rank by relevance. `--all` intersects per-keyword *session* sets — use it to pin down one
specific conversation from a handful of distinctive words that were never in the same
sentence.

### `recall list [flags]`

One row per session, newest activity first, with pin/summary state. Reads the sessions
registry (not the chunk index) and refreshes it from disk first, so **new sessions appear
without a reindex**.

| Flag | Default | Meaning |
|---|---|---|
| `--mode interactive\|exec\|all` | `interactive` | Which session kinds to include. Default hides non-interactive `codex exec`/headless runs. |
| `--source`, `--project`, `--since` | — | Same as search. |
| `--limit N` | `0` (all) | Max sessions. |
| `--pretty` | off | Table: `DATETIME · SOURCE · SESSION · PROJECT · TITLE · FLAGS`. Flags are 📌 pinned, ✶ has summary, ⚙ exec. |

### `recall pin <session-id>` / `recall unpin <session-id>`

Mark a session as pinned (surfaced first in the pinned group of a manager UI). `--source
claude|codex` is auto-detected from the index if omitted. Purely local bookkeeping in the
`pins` table.

### `recall summary <session-id> [flags]`

Generates a concise overview of a session — gist, key points, outcome, next steps — and
caches it in the DB, keyed on the transcript's mtime (so it regenerates when the session
grows).

> ⚠️ **This command sends transcript content off your machine.** It renders the session
> to plain text, caps it at **48,000 runes** (head + tail, middle elided), and pipes it
> into `codex exec` on stdin. Codex is normally network-backed by OpenAI, so the
> transcript goes wherever your Codex configuration sends it, and it costs whatever your
> Codex usage costs. This is the **only** command that does this.

| Flag | Meaning |
|---|---|
| `--refresh` | Ignore the cache and regenerate. |
| `--cached-only` | Return the cached summary if one exists (ignoring staleness); **never generate**. Empty summary if none cached — so glancing at a session never spends a codex call. |
| `--model NAME` | Codex model to use. Default: whatever codex is configured with. |
| `--pretty` | Print just the summary text instead of JSON. |

Requires an installed and authenticated `codex` CLI. Looked up on `PATH` and at
`/opt/homebrew/bin/codex`, `~/.local/bin/codex`, `/usr/local/bin/codex`.

### `recall resume <session-id>`

Prints the command that would resume a session, plus its working directory, as JSON:
`{source, session_id, cwd, command, path}`.

> ⚠️ **The emitted command disables the agent's safety rails.** It is
> `claude --dangerously-skip-permissions --resume <id>` for Claude sessions and
> `codex --dangerously-bypass-approvals-and-sandbox resume <id>` for Codex sessions —
> flags that let the resumed agent read, write, and execute without prompting you.
>
> `recall resume` **only prints this string. It never executes it.** Nothing runs unless
> you copy the command and run it yourself. If you'd rather resume with approvals intact,
> drop the dangerous flag and run `claude --resume <id>` / `codex resume <id>` by hand.
> If you pipe `recall resume` into a shell, you own that decision.

### `recall log [--limit N] [--pretty]`

Recent searches, newest first. **Every search is logged**, non-optionally, to the
`queries` table in the local DB: timestamp, query text, mode, filters, model, result
count, top sessions returned, latency. Written best-effort — a logging hiccup warns on
stderr but never fails your search.

It exists to power real-usage eval sets (vs. synthetic queries), query analytics, and
search history. Clear it with:

```sh
sqlite3 ~/.config/recall/index.db 'DELETE FROM queries'
```

## Privacy & network behavior

recall is **local-first, not local-only.** Precisely:

**Stays on your machine (with a default, local Ollama):**
- Reading and parsing transcripts.
- Chunking, embedding (Ollama at `http://localhost:11434`), and storing vectors.
- All search — semantic, lexical, hybrid, `--all`.
- The query log, pins, and the sessions registry.

**Leaves your machine:**
- **`recall summary`** pipes up to 48,000 runes of transcript into `codex exec`
  ([`sessions_cmd.go`](./sessions_cmd.go)). Codex is network-backed by OpenAI in normal
  use. If a session's content shouldn't reach a third-party API, don't summarize it.
  No other command calls out to anything.
- **A non-local `OLLAMA_HOST`.** It defaults to `http://localhost:11434`, but it's a plain
  env var — point it at a remote Ollama and every chunk you index and every query you
  type is sent to that server ([`internal/embed/ollama.go`](./internal/embed/ollama.go)).

**What the database actually holds.** `~/.config/recall/index.db` is not an opaque vector
blob. It stores **verbatim copies of your transcript text** (the `chunks` table, mirrored
into an FTS5 index), file paths, your full query history, pins, and generated summaries.
Treat it as being as sensitive as the transcripts themselves. Its directory is created
`0755` — on a shared machine, tighten it (`chmod 700 ~/.config/recall`).

**To delete everything recall created:**

```sh
rm -rf ~/.config/recall     # index, query log, pins, summaries — all of it
```

Deleting the index never touches your original transcripts in `~/.claude` / `~/.codex`.

## What is and isn't indexed

**Sources.** Fixed, built-in locations only — there is no config for custom transcript
roots, and `XDG_CONFIG_HOME` is not honored:
- `~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl`
- `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` and `~/.codex/archived_sessions/rollout-*.jsonl`
  (archived Codex sessions **are** indexed)

**Indexed:**
- Claude: `user` and `assistant` messages — `text` blocks **and `thinking` blocks**.
- Codex: `user` and `assistant` messages.

**Not indexed:**
- **Tool calls and tool results.** `tool_use` / `tool_result` blocks are explicitly
  skipped. Search finds the *conversation*, not the command output. (This is why a query
  for a filename you only ever saw in a tool result may come up empty.)
- Codex `developer` / `system` records.
- Claude sub-agent transcripts (`<session-id>/subagents/agent-*.jsonl`) — they carry the
  parent's session id and would create duplicate rows.
- Codex `logs_2.sqlite` (TRACE diagnostics) and legacy pre-2025 `.json` sessions.

**Indexed but hidden by default:** non-interactive runs (`codex exec`, `claude -p`/SDK).
Pass `--include-exec` to search them or `--mode exec|all` to list them.

## Config

| Env | Default | Purpose |
|---|---|---|
| `RECALL_EMBED_MODEL` | `qwen3-embedding:0.6b` | Embedding model **at index time**. See the caveat below. |
| `OLLAMA_HOST` | `http://localhost:11434` | Ollama server. Non-local values send your content off-box. |

⚠️ **`RECALL_EMBED_MODEL` does not control search.** At search time (without an explicit
`--model`), recall reads the model from the DB's own metadata — the model the index was
last built with — and ignores the env var. This is deliberate: a query must be embedded
with the same model as the vectors it's compared against, or cosine similarity is
meaningless. Precedence for search is `--model` → the index's recorded model. Precedence
for indexing is `--model` → `$RECALL_EMBED_MODEL` → `qwen3-embedding:0.6b`.

Index lives at `~/.config/recall/index.db`.

## Switching embedding models

Each file records the model that embedded it. `recall index --model NAME` re-embeds
**only** the files not already on that model — incrementally and resumably, not one big
reindex — so you can A/B models cheaply, and an interrupted migration just leaves a mix
that the next run finishes.

Semantic search only ranks chunks embedded with the query model; sessions still on
another model are skipped from semantic ranking (with a note on stderr) until you
re-embed them. Lexical and `--all` are model-agnostic and always see every chunk.

The default is `qwen3-embedding:0.6b`, chosen by a head-to-head eval against `bge-m3`
(both 1024-dim, so switching is drop-in) — see
[`docs/embedding-models.md`](./docs/embedding-models.md).

## Evaluation (and why you can't reproduce it)

[`eval/`](./eval/) holds the labeled benchmark that picked the default model: 22
natural-language queries, a harness that drives the real `recall` binary, and the
resulting Recall@k / MRR scores.

**It is not reproducible by anyone but the original author, by construction.** The gold
answers in `eval/fixture.json` are session-id prefixes from a *private local transcript
corpus*. You do not have those sessions, so on your machine every query scores zero. The
harness and the methodology are the reusable parts — the numbers are not independently
verifiable, and you should read them as a documented decision rather than a public
benchmark. To evaluate on your own corpus, replace `fixture.json` with queries and gold
session ids drawn from *your* index.

## Troubleshooting

**`ollama unreachable at http://localhost:11434 (is it running?)`**
Start it: `ollama serve`. If Ollama lives elsewhere, set `OLLAMA_HOST` — and note that a
remote host means your content leaves the machine.

**`ollama /api/embed 404: model "qwen3-embedding:0.6b" not found`**
`ollama pull qwen3-embedding:0.6b`. Confirm with `ollama list`.

**`index is empty — run 'recall index' first`**
Semantic/hybrid search needs a model recorded in the DB, which only `recall index`
writes. If `recall index` reported `0 sessions/files`, nothing was discovered — check that
`~/.claude/projects` or `~/.codex/sessions` actually exist and contain `.jsonl` files.

**Search returns nothing, but you know the session exists.**
In order of likelihood: (1) it's a `codex exec`/headless run — add `--include-exec`;
(2) the text you're searching for only ever appeared in a *tool result*, which isn't
indexed; (3) the session was indexed under a different embedding model — check for the
stderr note and re-run `recall index`; (4) `--project` / `--since` filtered it out.

**`note: N session(s) embedded under another model were excluded from semantic ranking`**
Your index is mixed. Run `recall index` to bring everything onto the current model, or
query the other model explicitly with `--model`.

**First index is very slow.** Expected — it embeds your entire history (roughly 5 chunks/s
for qwen3 on an M-series Mac). It's one-time; later runs only touch changed files.

**`codex CLI not found (needed for summaries)`**
Only `recall summary` needs it. Install and authenticate the `codex` CLI, or don't use
that command.

**Corrupt or incompatible index.** There's no repair path and no version negotiation — the
index is a derived artifact. Delete it and rebuild: `rm -rf ~/.config/recall && recall
index`. Schema upgrades that *can* be done in place (adding the per-file model columns)
happen automatically on open.

## Roadmap

- Reranker stage (`bge-reranker-v2-m3`) — the `Reranker` extension point is already wired
  in `internal/search`.
- `--full` to print the matching transcript span.
- Tests. There are none today; that's a gap, not a decision.

## Related

- [recall-app](https://github.com/ramsrib/recall-app) — desktop session manager built on
  this CLI. `list`, `pin`, `summary --cached-only`, and `resume` exist to serve it, and
  are equally usable on their own.

## License

MIT — see [`LICENSE`](./LICENSE).
