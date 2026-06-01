# recall

Local-first CLI that indexes your **Claude Code** and **Codex** session transcripts and
searches them with hybrid **semantic + full-text** ranking. Nothing leaves your machine —
embeddings are computed locally via Ollama.

See [`DESIGN.md`](./DESIGN.md) for the full design and the rationale behind every choice
(why SQLite over LanceDB/DuckDB/turbopuffer, why Go, why brute-force vectors, etc.).

## Documentation map

Returning cold? Start here:

| Doc | What it covers |
|---|---|
| [`DESIGN.md`](./DESIGN.md) | Architecture, schema, every design decision + the alternatives rejected. |
| [`docs/embedding-models.md`](./docs/embedding-models.md) | Which embedding model recall uses and **why** — the bge-m3 vs qwen3 comparison, results, and the decision. |
| [`docs/evaluating-embeddings-101.md`](./docs/evaluating-embeddings-101.md) | **Beginner's guide** — what embeddings are, how search is benchmarked, what Recall@k / MRR mean. Read this if the comparison doc feels jargon-heavy. |
| [`eval/`](./eval/) | The reproducible benchmark: query set (`fixture.json`), harness (`run.py`), last results (`results.json`). |

**Current state (2026-05-31):** default embedding model is `qwen3-embedding:0.6b` (it won a head-to-head
eval against `bge-m3` — see the embedding docs). The local index lives at `~/.config/recall/index.db`.

## Prerequisites

- Go 1.24+ (build only — the result is a single static binary, no cgo).
- [Ollama](https://ollama.com) running locally with an embedding model:
  ```sh
  ollama pull qwen3-embedding:0.6b
  ```

## Install

```sh
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

## Usage

```sh
# 1. Index your history (incremental — only reindexes sessions whose files changed).
#    Run it once, then re-run anytime to pick up new sessions; unchanged ones are skipped.
recall index

# 2. Search (hybrid: semantic + full-text, by default).
recall "how did I fix the pyexpat linking"

# Modes: hybrid (default) | semantic (meaning) | lexical (exact keyword / BM25)
recall --mode semantic "that vector db tradeoff discussion"
recall --mode lexical  "pyexpat"

# Search with a specific embedding model's vectors (must already be indexed
# under it). Defaults to whatever model the index currently uses.
recall --mode semantic "vector db tradeoffs" --model qwen3-embedding:0.6b

# Find the session containing ALL these keywords — even if they appear in
# DIFFERENT messages of the same session (session-level AND, pure keyword).
recall "webhook retry backoff timeout" --all

# Filters (combinable, and may go before or after the query)
recall "vector db tradeoffs" --source codex
recall "deploy script" --project backend --since 2026-05-01 --limit 10
recall "terraform state migration"                 # default: compact JSON
recall "terraform state migration" --pretty        # human-readable table
```

### `--all` vs the modes

- `--mode lexical` / `semantic` / `hybrid` match within a single chunk (one
  message span) and rank by relevance.
- `--all` is different: it asks "which session mentions *every* one of these
  words *somewhere*?" — intersecting the per-keyword session sets. Use it to
  pin down one specific conversation by a handful of distinctive words.

## Output

- **Default — compact, single-line JSON** (agent-first). Each object has
  `source, session_id, project, title, role, ts, path, score, snippet`, including
  `path` (the transcript file on disk) so an agent can open the matched session.
  No flag needed when calling from a script or agent.
- **`--pretty` — human table** (`DATETIME · SOURCE · SESSION · PROJECT · TITLE`). Session
  id is up front, since that's the value you copy to resume a session.

## Config

| Env | Default | Purpose |
|---|---|---|
| `RECALL_EMBED_MODEL` | `qwen3-embedding:0.6b` | Ollama embedding model (overridden by `--model`). |
| `OLLAMA_HOST` | `http://localhost:11434` | Ollama server. |

Index lives at `~/.config/recall/index.db`.

### Query log

**Every search is logged** (non-optional) to a `queries` table in the same local DB — timestamp,
query text, mode, filters, model used, result count, the top sessions returned, and latency. It's
written best-effort: a logging hiccup warns but never fails your search. This is the only data
recall *creates* (everything else just mirrors transcripts already on disk), and it never leaves
your machine.

```sh
recall log                 # recent searches as compact JSON (agent-first)
recall log --pretty        # human table: DATETIME · MODE · MODEL · N · ms · QUERY
recall log --limit 50      # how many to show (newest first)
```

It powers future use cases: real-usage eval sets (vs. synthetic queries), query analytics, and
search history. To clear it: `sqlite3 ~/.config/recall/index.db 'DELETE FROM queries'`.

### Switching embedding models

Each file records the model that embedded it. `recall index --model NAME` re-embeds **only**
the files not already on that model — incrementally and resumably, not one big reindex — so you
can A/B models cheaply. Semantic search only ranks chunks embedded with the query model; sessions
still on another model are skipped from semantic ranking (with a note on stderr) until you re-embed
them. Lexical / `--all` are unaffected.

The default is `qwen3-embedding:0.6b`, chosen by a head-to-head eval against `bge-m3` on this corpus
(both 1024-dim, so switching is drop-in):
- [`docs/embedding-models.md`](./docs/embedding-models.md) — the comparison, results, and recommendation.
- [`docs/evaluating-embeddings-101.md`](./docs/evaluating-embeddings-101.md) — **beginner's guide**:
  what embeddings are, how retrieval is benchmarked, and what Recall@k / MRR mean.

## Roadmap

- Reranker stage (`bge-reranker-v2-m3`) — extension point already wired in `internal/search`.
- `--full` to print the matching transcript span.
