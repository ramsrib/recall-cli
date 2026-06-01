#!/usr/bin/env python3
"""Compare embedding models for recall on a labeled query set.

Runs the real `recall` CLI against two indexes of the SAME corpus — one per
model — and reports Recall@k / MRR plus query latency. Each model lives in its
own $HOME so the per-model index DBs stay separate.

Usage:
  python3 eval/run.py \
    --bge-home "$HOME" \
    --qwen-home /tmp/recall-eval \
    --modes semantic hybrid
"""
import argparse, json, subprocess, time, statistics, sys

LIMIT = 30  # how deep we look for the gold session


def search(home, model, mode, query):
    """Return (ordered session-id prefixes, wall-clock seconds)."""
    cmd = ["recall", "--mode", mode, query, "--limit", str(LIMIT)]
    if model:
        cmd += ["--model", model]
    t0 = time.perf_counter()
    out = subprocess.run(cmd, capture_output=True, text=True,
                         env={"HOME": home, "PATH": __import__("os").environ["PATH"]})
    dt = time.perf_counter() - t0
    if out.returncode != 0:
        print(f"  ! recall failed: {out.stderr.strip()[:200]}", file=sys.stderr)
        return [], dt
    rows = json.loads(out.stdout or "[]")
    return [r["session_id"][:8] for r in rows], dt


def gold_rank(order, gold):
    """1-based rank of the best (earliest) gold session, or None."""
    ranks = [i + 1 for i, sid in enumerate(order) if sid in gold]
    return min(ranks) if ranks else None


def evaluate(name, home, model, mode, queries):
    rr, hits1, hits5, hits10, lats, misses = [], 0, 0, 0, [], []
    for q in queries:
        order, dt = search(home, model, mode, q["query"])
        lats.append(dt)
        rank = gold_rank(order, set(q["gold"]))
        if rank is None:
            rr.append(0.0); misses.append(q["query"])
            continue
        rr.append(1.0 / rank)
        hits1 += rank <= 1; hits5 += rank <= 5; hits10 += rank <= 10
    n = len(queries)
    return {
        "model": name, "mode": mode, "n": n,
        "recall@1": hits1 / n, "recall@5": hits5 / n, "recall@10": hits10 / n,
        "mrr": statistics.mean(rr), "p50_latency_ms": statistics.median(lats) * 1000,
        "misses": misses,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--fixture", default="eval/fixture.json")
    ap.add_argument("--bge-home", required=True)
    ap.add_argument("--qwen-home", required=True)
    ap.add_argument("--modes", nargs="+", default=["semantic", "hybrid"])
    args = ap.parse_args()

    fx = json.load(open(args.fixture))
    queries = fx["queries"]

    # Preflight: warn on duplicate gold prefixes across queries (not fatal).
    backends = [
        ("bge-m3", args.bge_home, "bge-m3"),
        ("qwen3-0.6b", args.qwen_home, "qwen3-embedding:0.6b"),
    ]

    results = []
    for mode in args.modes:
        for name, home, model in backends:
            print(f"running {name} / {mode} over {len(queries)} queries…", file=sys.stderr)
            results.append(evaluate(name, home, model, mode, queries))

    # Report
    print(f"\n{'model':<12}{'mode':<10}{'R@1':>7}{'R@5':>7}{'R@10':>7}{'MRR':>8}{'p50 ms':>9}")
    print("-" * 60)
    for r in results:
        print(f"{r['model']:<12}{r['mode']:<10}{r['recall@1']:>7.2f}{r['recall@5']:>7.2f}"
              f"{r['recall@10']:>7.2f}{r['mrr']:>8.3f}{r['p50_latency_ms']:>9.0f}")
    print()
    for r in results:
        if r["misses"]:
            print(f"{r['model']}/{r['mode']} missed (gold not in top {LIMIT}): "
                  + "; ".join(m[:50] for m in r["misses"]))
    json.dump(results, open("eval/results.json", "w"), indent=2)
    print("\nwrote eval/results.json")


if __name__ == "__main__":
    main()
