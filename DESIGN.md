# recall — design

A local-first CLI that indexes **Claude Code** and **Codex** session transcripts and
searches them with **hybrid (semantic + full-text)** ranking. Everything runs on-machine;
no conversation content ever leaves the laptop.

> Status: scratch tryout. Two commands: `recall index` and `recall <query>`.

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
- Path: `~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl`
  (folder name = working dir with `/`→`-`, so **project is the folder name decoded**).
- Records of interest:
  - `user` / `assistant` — the conversation. `user.message.content` is a string or a list
    (with `tool_result`); `assistant.message.content` is a list of blocks (`text`, `thinking`, `tool_use`).
  - `ai-title` (`aiTitle`) — human-readable session title.
  - Every record has `timestamp`, `sessionId`, `cwd`, `gitBranch`.

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

```sql
CREATE TABLE files (              -- the freshness manifest
  path        TEXT PRIMARY KEY,
  source      TEXT NOT NULL,      -- 'claude' | 'codex'
  mtime_ns    INTEGER NOT NULL,   -- stat mtime in unix nanos
  size        INTEGER NOT NULL,
  indexed_at  INTEGER NOT NULL,
  embed_model TEXT                -- model that produced this file's vectors
);

CREATE TABLE chunks (
  id          INTEGER PRIMARY KEY,
  file_path   TEXT NOT NULL,      -- FK → files.path (manual cascade on reindex)
  source      TEXT NOT NULL,
  session_id  TEXT,
  project     TEXT,
  title       TEXT,
  ts          INTEGER,            -- unix seconds
  role        TEXT,
  text        TEXT NOT NULL,
  embedding   BLOB,               -- float32 little-endian, len = dim*4
  embed_model TEXT                -- denormalized from files; indexed, used to
);                                -- gate the semantic candidate set by model

CREATE VIRTUAL TABLE chunks_fts USING fts5(text, content='chunks', content_rowid='id');
-- + insert/delete triggers to keep chunks_fts in sync

CREATE TABLE queries (             -- query log: every search is recorded here
  id           INTEGER PRIMARY KEY,
  ts           INTEGER NOT NULL,   -- when it ran (unix seconds)
  query        TEXT NOT NULL,
  mode         TEXT NOT NULL,      -- hybrid|semantic|lexical|all
  source, project, since, lim,     -- the filters used
  model        TEXT,               -- embedding model ('' for lexical/all)
  n_results    INTEGER NOT NULL,
  top_sessions TEXT,               -- JSON ["source/session_id", …], best first
  duration_ms  INTEGER
);
```

The **query log** is the one piece of data recall *creates* (everything else mirrors transcripts
already on disk). It's written best-effort after every search — non-optional, but a logging error
only warns and never fails the search. It stays in the same local DB; nothing leaves the machine.
It exists to power future work: real-usage eval sets (vs. synthetic queries — see
[`docs/evaluating-embeddings-101.md`](./docs/evaluating-embeddings-101.md)), query analytics, and
"what did I search for" history. Inspect with `recall log`; clear with
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
     embed each chunk via Ollama (prefix "search_document: ")
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
  main.go                 CLI: arg parse + dispatch (stdlib flag)
  internal/
    store/    SQLite open, schema, manifest, chunk CRUD, FTS + vector queries
    adapter/  Record type; claude.go, codex.go; file discovery
    embed/    Ollama client (batch embed + query embed)
    index/    incremental orchestration
    search/   semantic + lexical + RRF fusion, result collapsing
```

## Open / later
- Tune chunk size for retrieval quality.
- Embed conditioning is now **model-aware** (`internal/embed`): bge/nomic use the
  `search_document:`/`search_query:` prefixes; qwen3 uses bare docs + an `Instruct:` query.
  Still untested: whether bge-m3 does better *without* its prefixes (it doesn't strictly need them).
- Model comparison (bge-m3 vs qwen3-embedding:0.6b) measured in [`docs/embedding-models.md`](./docs/embedding-models.md):
  qwen3 ranks #1 more often at ~2× index cost — **made the default 2026-05-31**. N=22 was small, so
  revisit with a larger eval (see that doc's "What would make this conclusive").
- Optional `--full` to print the matching transcript span.
- Reranker stage (`bge-reranker-v2-m3`) — extension point already wired in `internal/search`.
- If it graduates to `active/`: consider `sqlite-vec` only if scale demands ANN.

### Next step: search agent memory (not just transcripts)

Index curated agent **memory** alongside session transcripts. As of writing,
~58 Claude Code memory files exist (`~/.claude/projects/*/memory/*.md` —
`feedback_*`, `project_*`, `user_role`, plus `MEMORY.md` indexes); Codex memory
(`~/.codex/memories/`) is empty so far.

Design — memory is just a **third adapter**, no new index or pipeline:
- `adapter.Discover()` also walks `~/.claude/projects/*/memory/*.md`; a small
  `parseMemory` yields one Record per file (whole file = one chunk — they're
  small), `Source: "memory"`, project from the path, title from filename / first
  `#` heading. Incremental mtime indexing + embeddings + hybrid search all reuse.
- Expose ergonomically: `recall "X" --source memory`, with `recall memory "X"`
  as a shorthand subcommand. Default search includes memory — ~58 small,
  high-signal docs surface relevant facts without drowning the 7k+ transcript
  chunks.
- Memory is a distinct *kind* of content (curated, always-current, high
  signal-to-noise) vs raw conversation — hence the dedicated source/filter.
- Codex memory: add the same way once `~/.codex/memories/` is populated. Its
  sqlite stores (`goals_1`, `state_5`) are operational state, not searchable notes.
- "Cloud" memory (claude.ai web history, Claude cloud memory): not on disk —
  would need an export/API path; out of scope until a source is identified.
