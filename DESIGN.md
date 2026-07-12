# recall — design

A local-first CLI that indexes **Claude Code** and **Codex** session transcripts and
searches them with **hybrid (semantic + full-text)** ranking.

**Local-first, not local-only.** Indexing and every search path run on-machine against a
local Ollama, and no conversation content leaves the box on those paths. Two escape
hatches exist and are deliberate:

- `recall summary` pipes transcript text (capped at 48k runes) into `codex exec`, which
  is network-backed. It is the only command that does.
- `OLLAMA_HOST` is a plain env var; pointing it at a remote Ollama sends every chunk and
  query to that server.

Both are documented in the README's *Privacy & network behavior* section. The design
below concerns the local path.

> Status: working tool, no tests. Eight commands: `index`, `<query>`, `log`, `list`,
> `pin`, `unpin`, `summary`, `resume` (see `main.go`).

---

## Why this exists

Both Claude Code and Codex write every session to disk as JSONL. That history is a goldmine
("how did I fix the pyexpat linking?", "what was that turbopuffer tradeoff I reasoned through?")
but there's no way to search across it. `recall` indexes both tools into one store and answers
natural-language or keyword queries from the terminal.

---

## Decisions (and the alternatives we rejected)

| Decision | Choice | Why / what we rejected |
|---|---|---|
| Language | **Go** | Single binary, instant cold start (agents invoke it), no venv. |
| SQLite driver | **`modernc.org/sqlite`** (pure Go) | cgo-free → trivial cross-compile + small static binary. Rejected `mattn` (cgo). |
| Store | **SQLite** (one `index.db`) | One file; trivial per-file delete/replace for incremental reindex; FTS5 built in. |
| — vs LanceDB | rejected | No first-class Go SDK; embedding it = DIY Rust FFI. Its scale/hybrid wins don't apply at ~80k vectors. |
| — vs DuckDB | rejected | cgo + fat binary; FTS index is a *snapshot* (must rebuild on change) — fights incremental indexing; HNSW persistence experimental. OLAP engine, our workload is point lookup. |
| — vs turbopuffer | rejected | Cloud. Would ship private session content off-machine; solves a million-vector scale problem we don't have. |
| Vector search | **brute-force cosine in Go** | ~80k vectors × 1024d → well under 100ms. No ANN index needed; avoids `sqlite-vec` (cgo). |
| Full-text | **SQLite FTS5** (BM25) | Built into modernc; auto-updates via triggers (incremental-friendly). |
| Hybrid merge | **Reciprocal Rank Fusion (RRF)** | Standard way to blend the two ranked lists; ~15 lines, no single-score magic. |
| Embeddings | **Ollama `qwen3-embedding:0.6b`** (1024d, local) | Default since 2026-05-31: beat `bge-m3` on a labeled eval of this corpus (better top-1 / MRR, same 1024d + query latency) — see [`docs/embedding-models.md`](./docs/embedding-models.md). Local, private, free, no API key; 32k context. `bge-m3` remains available via `--model` (~2× faster to index). Dim is dynamic — swapping models only requires a reindex. |

### When these choices would flip
- Move to Python/TS/Rust, or grow to millions of chunks → **LanceDB** (native hybrid + rerankers).
- Want analytics over history + accept cgo → **DuckDB**.
- Multi-user / huge shared corpus → **turbopuffer**.

---

## Data sources

Both are JSONL; both carry enough metadata for project attribution.

### Claude Code
- Path: `~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl` (the folder name is the
  working dir with `/`→`-`). We do **not** decode that folder name: `Record.Project` is
  each message's own `cwd` field, read straight from the record (`claude.go`). A
  transcript with no messages therefore yields no project at all — we never fall back to
  deriving it from the path.
- Records of interest:
  - `user` / `assistant` — the conversation. `user.message.content` is a string or a list
    of blocks; `assistant.message.content` is a list of blocks (`text`, `thinking`,
    `tool_use`).
  - **Only `text` and `thinking` blocks are indexed.** `tool_use` and `tool_result` are
    explicitly skipped (`extractClaudeContent`), so the index covers the conversation, not
    the command output. Assistant reasoning *is* searchable; tool output is not.
  - `ai-title` (`aiTitle`) — human-readable session title.
  - Every record has `timestamp`, `sessionId`, `cwd`, `gitBranch`.
- Sub-agent transcripts (`<session-id>/subagents/agent-*.jsonl`) are skipped: they carry
  the *parent's* session id and would produce duplicate session rows.

### Codex
- Path: `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` and `~/.codex/archived_sessions/rollout-*.jsonl`.
- First line `session_meta.payload` → `cwd`, `git.branch`, `timestamp`, `id`.
- `response_item.payload` where `payload.type == "message"` → `role` (user/assistant/...) and
  `content` = list of `{type: input_text|output_text|..., text}` blocks.
- `logs_2.sqlite` is TRACE diagnostics — **ignored**.
- Legacy pre-2025 `.json` (non-`.jsonl`) sessions — skipped for v1.

Both adapters normalize to one record:

```
Record { Source, SessionID, Project, Title, Timestamp, Role, Text }
```

---

## Store schema (`~/.config/recall/index.db`)

Opened with `busy_timeout=5000` (set *first*, so it covers the WAL switch below),
`journal_mode=WAL`, `synchronous=NORMAL`. Authoritative source: `internal/store/store.go`.

```sql
CREATE TABLE meta (               -- key/value; holds embed_model = the index's
  key   TEXT PRIMARY KEY,         -- current model, i.e. the default a query embeds with
  value TEXT
);

CREATE TABLE files (              -- the freshness manifest
  path        TEXT PRIMARY KEY,
  source      TEXT NOT NULL,      -- 'claude' | 'codex'
  mtime_ns    INTEGER NOT NULL,   -- stat mtime in unix nanos
  size        INTEGER NOT NULL,
  indexed_at  INTEGER NOT NULL,
  embed_model TEXT                -- model that produced this file's vectors
);

CREATE TABLE chunks (             -- the search index (a derived artifact)
  id          INTEGER PRIMARY KEY,
  file_path   TEXT NOT NULL,      -- FK → files.path (manual cascade on reindex)
  source      TEXT NOT NULL,
  session_id  TEXT,
  project     TEXT,
  title       TEXT,
  ts          INTEGER,            -- unix seconds
  role        TEXT,
  text        TEXT NOT NULL,      -- verbatim transcript text
  embedding   BLOB,               -- float32 little-endian, len = dim*4
  embed_model TEXT                -- denormalized from files; indexed, used to
);                                -- gate the semantic candidate set by model
-- idx_chunks_file, idx_chunks_source, idx_chunks_ts, idx_chunks_model

CREATE VIRTUAL TABLE chunks_fts USING fts5(text, content='chunks', content_rowid='id');
-- + insert/delete triggers to keep chunks_fts in sync

CREATE TABLE sessions (           -- the listing registry: one row per session FILE.
  path       TEXT PRIMARY KEY,    -- Separate from `chunks` so `recall list` reflects
  source     TEXT NOT NULL,       -- new sessions without a reindex; refreshed from disk
  session_id TEXT NOT NULL,       -- with (mtime_ns, size) as the cache key, so unchanged
  project    TEXT,                -- files cost only a stat.
  title      TEXT,
  first_ts   INTEGER,
  last_ts    INTEGER,
  msg_count  INTEGER,
  mode       TEXT,                -- 'interactive' | 'exec'
  mtime_ns   INTEGER NOT NULL,
  size       INTEGER NOT NULL
);                                -- idx_sessions_sid, idx_sessions_last

CREATE TABLE pins (               -- pinned sessions (manager UX)
  source     TEXT NOT NULL,
  session_id TEXT NOT NULL,
  pinned_at  INTEGER NOT NULL,    -- drives most-recently-pinned-first ordering
  PRIMARY KEY(source, session_id)
);

CREATE TABLE summaries (          -- `recall summary` output, cached per session
  source        TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  file_mtime_ns INTEGER NOT NULL, -- invalidates the cache when the transcript grows
  model         TEXT,
  summary       TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  PRIMARY KEY(source, session_id)
);

CREATE TABLE queries (            -- query log: every search is recorded here
  id           INTEGER PRIMARY KEY,
  ts           INTEGER NOT NULL,  -- when it ran (unix seconds)
  query        TEXT NOT NULL,
  mode         TEXT NOT NULL,     -- hybrid|semantic|lexical|all
  source TEXT, project TEXT, since INTEGER, lim INTEGER,   -- the filters used
  model        TEXT,              -- embedding model ('' for lexical/all)
  n_results    INTEGER NOT NULL,
  top_sessions TEXT,              -- JSON ["source/session_id", …], best first
  duration_ms  INTEGER
);                                -- idx_queries_ts
```

### What recall creates vs. mirrors

`chunks` / `chunks_fts` / `files` / `sessions` are **derived**: delete them and a reindex
rebuilds them from the transcripts on disk. But recall does create data that exists
nowhere else, and it all lives in this same DB:

- **`queries`** — your search history (below).
- **`summaries`** — genuinely new content, generated by a model, not a copy of anything.
- **`pins`** — your annotations.
- **`meta`** — the index's current embedding model.

So "recall only mirrors what's already on disk" is *false*. `rm -rf ~/.config/recall`
deletes all of it; nothing else is written outside that directory. Note also that `chunks`
holds transcript text **verbatim** — the DB is as sensitive as the transcripts it indexes.

The **query log** is written best-effort after every search — non-optional, but a logging
error only warns and never fails the search. It exists to power future work: real-usage
eval sets (vs. synthetic queries — see
[`docs/evaluating-embeddings-101.md`](./docs/evaluating-embeddings-101.md)), query
analytics, and "what did I search for" history. Inspect with `recall log`; clear with
`sqlite3 ~/.config/recall/index.db 'DELETE FROM queries'`.

---

## `recall index` — incremental, both sources

```
1. Enumerate session files from both adapters.
2. For each file, stat() → (mtime_ns, size) and compare against the manifest:
     • not in files                       → NEW       → index
     • mtime/size differ                  → CHANGED   → reindex
     • content same, embed_model differs  → REMODELED → re-embed (model switch)
     • content same, embed_model same     → SKIP
3. Files in table but missing on disk → delete their chunks + row.
4. Indexing one file (one txn):
     DELETE FROM chunks WHERE file_path=?      (FTS5 trigger cleans the index)
     adapter parse → records → chunk to ~512-token windows
     embed each chunk via Ollama (conditioning is model-aware — see below)
     INSERT chunks(... embed_model = current model)
     UPSERT files(path, source, mtime_ns, size, indexed_at=now, embed_model)
```

Re-runs after a few new sessions touch only those files. Embedding dominates first-run time
(one-time); incremental runs are near-instant.

**Embedding-model tracking.** Each file (and its chunks) records the model that produced its
vectors. `meta.embed_model` holds the *current* model — the one a query embeds with by default
(`--model` overrides). Two consequences:

- **Model switches are per-file, incremental, and resumable.** Running `recall index --model X`
  re-embeds only files not already on `X`; it is no longer one global all-or-nothing wipe. An
  interrupted migration just leaves a mix of models, and the next run finishes it.
- **The semantic path ranks only same-model vectors.** Cosine across two models' spaces is
  meaningless, so `Candidates` filters by the query model; sessions embedded under another model
  are excluded from semantic/hybrid ranking (with a stderr note) until re-embedded. Lexical/`--all`
  are model-agnostic and always see every chunk. A pre-`embed_model` DB is migrated in place and
  backfilled from the old global `meta.embed_model`.

---

## `recall <query>` — search

```
recall "QUERY" [--mode hybrid|semantic|lexical] [--source claude|codex]
               [--project SUBSTR] [--since YYYY-MM-DD] [--limit N] [--model NAME] [--all] [--pretty]
recall log [--limit N] [--pretty]              # recent searches from the query log
```

```
1. Apply SQL filters (source, project LIKE, ts>=since) to narrow candidates.
2. Semantic: embed query with the query model (--model, else the index's current model), using
             that model's recipe (bge → "search_query: " prefix; qwen3 → "Instruct: …\nQuery: …")
             → cosine vs candidate embeddings *of that same model* → list.
   Lexical:  chunks_fts MATCH query → bm25() → ranked list.
   Hybrid:   RRF-merge the two lists (k=60).
   --all:    intersect per-keyword session sets (session-level AND, lexical).
3. Collapse to one row per session (best-scoring chunk wins).
4. Log the search to the `queries` table (best-effort; never fails the search).
5. Output: compact single-line JSON by default (fields incl. session_id + path);
   --pretty → human table (datetime · source · session · project · title).
```

Default mode is **hybrid**.

---

## Layout

```
recall/
  main.go                 CLI: arg parse + dispatch (stdlib flag); index, search, log
  sessions_cmd.go         session-manager commands: list, pin/unpin, summary, resume
  internal/
    store/    SQLite open, schema, manifest, chunk CRUD, FTS + vector queries,
              sessions registry, pins, summaries, query log
    adapter/  Record type; claude.go, codex.go; file discovery; interactive/exec mode
    embed/    Ollama client (batch embed + query embed, model-aware conditioning)
    index/    incremental orchestration + sessions-registry refresh
    search/   semantic + lexical + RRF fusion, result collapsing, Reranker hook
```

---

## Session-manager commands

`list`, `pin`/`unpin`, `summary`, and `resume` exist so that a UI (the
[recall-app](https://github.com/ramsrib/recall-app) desktop client) can be a thin shell
over this binary rather than reimplementing the store. They're equally usable on their own.

The design consequence worth recording: **the `sessions` registry is separate from
`chunks`.** Listing sessions must not require a reindex — a session that was created
thirty seconds ago should appear in `recall list` immediately, and a search index is far
too expensive to keep that fresh. So `RefreshSessions` reconciles one row per session file
against disk on every `list`, keyed on `(mtime_ns, size)`, which means unchanged files cost
a single `stat`. The chunk table is now purely a *search* artifact.

Two other decisions in this group:

- **`summary --cached-only`** exists because a UI renders session metadata on every
  click. Generation calls `codex exec` (network, costs money), so the app must be able to
  ask "do you already have one?" without ever triggering a call. Cache validity is keyed
  on the transcript's mtime, except in `--cached-only` mode, where staleness is ignored —
  an actively-growing session would otherwise look stale on every open.
- **`resume` prints, never executes.** It emits the resume command as a JSON string. The
  command it emits carries `--dangerously-skip-permissions` (Claude) or
  `--dangerously-bypass-approvals-and-sandbox` (Codex) — appropriate for a deliberate
  human "put me back where I was", never for something a tool runs on your behalf. The
  decision to run it stays with the caller.

---

## Open / later
- Tune chunk size for retrieval quality.
- Embed conditioning is **model-aware** (`internal/embed`): bge/nomic use the
  `search_document:`/`search_query:` prefixes; qwen3 uses bare docs + an `Instruct:` query.
  Still untested: whether bge-m3 does better *without* its prefixes (it doesn't strictly need them).
- Model comparison (bge-m3 vs qwen3-embedding:0.6b) measured in [`docs/embedding-models.md`](./docs/embedding-models.md):
  qwen3 ranks #1 more often at ~2× index cost — it is the default. N=22 was small, so
  revisit with a larger eval (see that doc's "What would make this conclusive").
- Optional `--full` to print the matching transcript span.
- Reranker stage (`bge-reranker-v2-m3`) — extension point already wired in `internal/search`.
- Tests. There are none. Parsers, chunking, the search modes, and the migration path all
  want them before this is trustworthy to change.
- `sqlite-vec` (ANN) only if a corpus ever grows past what brute-force cosine handles
  comfortably — well north of 100k chunks. Not a problem today.

### Next step: search agent memory (not just transcripts)

Both tools can keep curated **memory** files alongside raw transcripts
(`~/.claude/projects/*/memory/*.md`, `~/.codex/memories/`). These are a distinct *kind* of
content — curated, current, high signal-to-noise — and worth indexing next.

Design — memory is just a **third adapter**, no new index or pipeline:
- `adapter.Discover()` also walks the memory directories; a small `parseMemory` yields one
  Record per file (whole file = one chunk — they're small), `Source: "memory"`, project
  from the path, title from filename / first `#` heading. Incremental mtime indexing,
  embeddings, and hybrid search all reuse the existing machinery.
- Expose ergonomically: `recall "X" --source memory`, with `recall memory "X"` as a
  shorthand subcommand. Default search includes memory: memory files are few and small
  relative to a transcript corpus, so they surface relevant facts without drowning it.
- Codex `~/.codex/memories/`: same adapter. Its sqlite stores (`goals_1`, `state_5`) are
  operational state, not searchable notes — skip them.
- "Cloud" memory (claude.ai web history, Claude cloud memory): not on disk — would need an
  export/API path; out of scope until a source is identified.
