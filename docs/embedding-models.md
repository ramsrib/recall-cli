# Embedding models: `bge-m3` vs `qwen3-embedding:0.6b`

recall's semantic ranking is only as good as the local embedder behind it. Both models below
are **drop-in for recall's schema** — they emit **1024-dim** vectors, so switching is just a
re-index (now incremental and per-file; see [`../DESIGN.md`](../DESIGN.md) → *Embedding-model
tracking*). This doc compares the two candidates and records what we'd need to pick between them
**with data** rather than vibes.

> **New to embeddings / retrieval metrics?** Read
> [`evaluating-embeddings-101.md`](./evaluating-embeddings-101.md) first — it explains what an
> embedding is, how a search benchmark works, and what Recall@k / MRR mean, then comes back here for
> the numbers.

> **Status: measured (2026-05-31).** Ran a 22-query labeled eval over both models indexed on the
> full corpus, each with its own recommended prompt recipe. **`qwen3-embedding:0.6b` ranks the
> right session first more often** (Recall@1 0.82 vs 0.73, MRR 0.87 vs 0.79 semantic) at **~2×
> the indexing cost** and comparable query latency. Both find the answer within the top 5 equally
> often. See [Results](#results-2026-05-31). N is small — treat the edge as suggestive, not proven.

## TL;DR

- **`qwen3-embedding:0.6b` is the current default** (switched 2026-05-31 after the eval below). Same
  1024 dims as bge-m3 (so same cosine cost and memory), a newer architecture, 4× the context window,
  and instruction-aware queries.
- **`bge-m3` is the previous default** — mature, well-understood, strong on mixed
  code+prose+multilingual content, and ~2× faster to index. Still a fine choice via `--model bge-m3`.
- **Measured verdict:** on our own data `qwen3-embedding:0.6b` wins on top-1 precision and MRR in
  both modes; the two tie on Recall@5/@10. It costs ~2× the indexing time (one-time / incremental)
  but is a smaller model on disk and queries just as fast. **Switched the default — but N is small,
  so confirm with a larger eval.** Numbers and method in [Results](#results-2026-05-31).

## Comparison

| | `bge-m3` | `qwen3-embedding:0.6b` |
|---|---|---|
| Vendor / year | BAAI, 2024 | Alibaba Qwen, 2025 |
| Params | ~560M (XLM-RoBERTa-large) | ~600M |
| **Output dim** | **1024** (fixed) | **1024** ✓ verified (Matryoshka — truncatable down to 32) |
| Max context | 8,192 tokens | 32,768 tokens |
| Multilingual | Yes (100+ langs) | Yes (100+ langs) |
| Query conditioning | static prefix (`search_query:`) | **instruction-aware** (`Instruct: {task}\nQuery: …`) |
| Extra retrieval modes | dense + sparse + ColBERT multi-vector¹ | dense only |
| On *our* corpus² | R@1 0.73 · MRR 0.79 (semantic) | **R@1 0.82 · MRR 0.87** (semantic) |
| Model size on disk | 1.2 GB | **639 MB** |
| Index throughput (this Mac) | **~9.7 chunks/s** | ~4.9 chunks/s |
| Pulls cleanly in Ollama | `ollama pull bge-m3` | `ollama pull qwen3-embedding:0.6b` |

¹ recall only uses the **dense** vector from bge-m3; its sparse/ColBERT modes aren't wired up.
² Measured on a 22-query labeled set over the full corpus (2026-05-31) — see [Results](#results-2026-05-31).

### Why the dims matching matters here

recall does **brute-force cosine in Go** over every candidate vector. Both models are 1024-dim, so:

- index size, RAM, and per-query cosine cost are **identical** between them, and
- switching requires **no schema change** — only a re-embed.

A larger model (e.g. `qwen3-embedding:4b` at 2560-dim or `:8b` at 4096-dim) would raise quality
but also inflate the index and slow every brute-force scan proportionally. For recall's
local-first, no-ANN design, **staying at 1024 dims is the constraint that makes `0.6b` the right
size class to compare against bge-m3** — not the bigger Qwen variants.

## ⚠️ Fairness caveat: recall's prefixes are tuned for bge/nomic

`internal/embed/ollama.go` hardcodes:

```go
docPrefix   = "search_document: "
queryPrefix = "search_query: "
```

These are the **bge/nomic** asymmetric-retrieval convention. Qwen3-Embedding expects a different
recipe: **no prefix on documents**, and an **instruction** prepended to queries
(`Instruct: <task description>\nQuery: <text>`). Qwen reports a few-percent retrieval gain from
using the instruction format.

So a head-to-head today would **under-sell qwen3** — it'd be embedding with the wrong prompt
convention. A fair comparison must make the prefix/instruction scheme **model-aware** first (e.g. a
per-model strategy keyed off `Client.Model`). That work is part of the TODO below.

---

## Smoke test (2026-05-31): mechanics verified

Indexed a single session under `qwen3-embedding:0.6b` in an isolated `$HOME` (real index
untouched) to confirm the per-model tracking added in `internal/{store,index,search}` actually
works with this model. **This validated plumbing, not retrieval quality.**

| Check | Result |
|---|---|
| Output dimension | **1024** (4096-byte float32 blobs) — confirmed drop-in, no schema change |
| Embeddings non-degenerate | relevant query cosine **0.81** vs off-topic **0.40** — meaningfully discriminating |
| Per-model storage | `chunks.embed_model` / `files.embed_model` = `qwen3-embedding:0.6b` as expected |
| Query-time gating | querying with `--model bge-m3` against a qwen3-only index → **0 results + exclusion note** |
| Incremental switch | re-index same model → *skipped*; switch model → *1 re-embedded (model change)* |

What this does **not** tell us: whether qwen3 *ranks better* than bge-m3 on real queries. That
needs the labeled eval below. Note also the smoke test embedded qwen3 with the bge-style
`search_query:` prefix (the current code), i.e. the unfair convention flagged above.

---

## Results (2026-05-31)

Both models indexed over the **full corpus** (~11.3k chunks / ~213 sessions), each embedded with
**its own recommended recipe** (bge: `search_document:`/`search_query:` prefixes; qwen3: bare
documents + `Instruct:` queries — implemented in `internal/embed/ollama.go`). Evaluated on a
**22-query labeled set** ([`eval/fixture.json`](../eval/fixture.json)) of natural-language queries
paraphrased from known sessions; a query "hits" if a gold session lands in the top *k*.

### Retrieval quality

| model | mode | Recall@1 | Recall@5 | Recall@10 | MRR |
|---|---|---|---|---|---|
| bge-m3 | semantic | 0.73 | 0.91 | 0.95 | 0.793 |
| **qwen3-embedding:0.6b** | semantic | **0.82** | 0.91 | 0.95 | **0.869** |
| bge-m3 | hybrid | 0.73 | 0.91 | 0.95 | 0.808 |
| **qwen3-embedding:0.6b** | hybrid | **0.77** | 0.91 | 0.95 | **0.825** |

- **qwen3 puts the right session at #1 more often** (+9 pts R@1 semantic, +4 hybrid) and has higher
  MRR in both modes. **Recall@5 and @10 are identical** — both models find the answer within the
  first few results equally well; qwen3's edge is purely in *ordering* the top of the list.
- Hybrid narrows qwen3's lead (BM25 carries part of the signal, as predicted).
- **One shared miss**: "broken homebrew formula upgrades…" — neither model ranked the gold session
  in the top 30. Same failure for both, so it's a corpus/ambiguity issue, not a model difference.

### Cost

| | bge-m3 | qwen3-embedding:0.6b |
|---|---|---|
| Index throughput (256-chunk batch, warm) | ~9.7 chunks/s | ~4.9 chunks/s (**~2× slower**) |
| Full-corpus re-embed (~11.3k chunks) | — | ~21 min |
| Query latency (p50, embed + brute-force cosine) | ~250–300 ms | ~240–275 ms (comparable) |
| Model size on disk | 1.2 GB | 639 MB |
| Index size | identical — both 1024-dim float32 | identical |

Indexing is the only place qwen3 costs more, and it's one-time / incremental (per-file; see
`DESIGN.md`). Query-time cost is dominated by the brute-force cosine scan, which is identical at
1024 dims, so day-to-day search feels the same.

### Recommendation — adopted

**Switched the default to `qwen3-embedding:0.6b`** (2026-05-31): `embed.DefaultModel` changed in
`internal/embed/ollama.go`, README/DESIGN updated, live index re-embedded under qwen3. It ranks
better where it matters for both an agent and a human (the *first* result), queries just as fast,
and is a smaller model on disk; the only cost is a slower one-time re-index. **Caveat: N=22 is
small** — the R@1 gap is 2 flipped queries — so this is a directional result consistent with public
benchmarks, not a proven one. Revisit with a larger eval (below). To go back: `--model bge-m3`.

### Reproduce

```sh
# 1. Index each model over the same corpus (separate $HOME → separate index DB).
#    The live index is qwen3 now; build a bge-m3 copy under a throwaway HOME instead.
EVAL=/tmp/recall-bge; mkdir -p "$EVAL"
ln -s ~/.claude "$EVAL/.claude"; ln -s ~/.codex "$EVAL/.codex"
HOME="$EVAL" recall index --model bge-m3

# 2. Run the eval (Recall@k, MRR, p50 latency) across both.
#    --qwen-home is the live index (now qwen3); --bge-home is the throwaway bge copy.
python3 eval/run.py --bge-home "$EVAL" --qwen-home "$HOME" --modes semantic hybrid
```

### What would make this conclusive

- [ ] **Bigger labeled set** (50–100 queries), ideally drawn from real typed queries rather than
      Claude-authored paraphrases — the current single-annotator fixture can bias toward either model.
      recall now logs every search (`recall log`), so a real-usage query set can be harvested over time
      instead of hand-written.

  > **⏳ Revisit when `recall log` has ~100+ real queries.** Then redo the eval properly:
  > 1. Export the logged queries: `sqlite3 ~/.config/recall/index.db "SELECT query FROM queries"`.
  > 2. Sample ~100 (dedup near-identical ones; keep the natural mix of query types).
  > 3. **Label them** — real queries fix *realism* but carry **no ground truth**. Don't reuse the
  >    logged `top_sessions` as labels (circular — that grades a model against its own output).
  >    Instead pool both models' results and judge relevance blind (see
  >    [evaluating-embeddings-101 §4](./evaluating-embeddings-101.md#real-queries-help-with-realism-not-labels)).
  > 4. Re-run `eval/run.py` on the new fixture; add nDCG + bootstrap CIs.
  >
  > **Bigger lever:** if recall ever logs *which result you opened* after a search, that selection is
  > an implicit relevance label — the query log becomes a self-labeling eval set with no manual
  > judging (how search engines eval via click-through). Not captured today; worth adding first.
- [ ] **nDCG@10** and significance testing (the N=22 R@1 gap is within noise).
- [ ] Slice by **query type** (code vs prose vs exact-identifier vs multilingual) — averages hide
      where each model wins.
- [ ] Re-test bge-m3 with *its* best query recipe too (it currently uses the shipped prefixes; a
      bare-text or instruction variant might move its numbers).
