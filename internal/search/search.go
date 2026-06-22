// Package search ranks chunks by semantic similarity, full-text BM25, or a
// reciprocal-rank-fusion of both, then collapses to one hit per session.
//
// Reranker hook: Run accepts an optional Reranker. After fusion produces the
// top candidates, a cross-encoder reranker (e.g. bge-reranker-v2-m3 served
// locally) can reorder them for higher precision. The Identity reranker is the
// current no-op; swap in a real one without touching callers.
package search

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"recall/internal/adapter"
	"recall/internal/embed"
	"recall/internal/store"
)

// keepSession reports whether a session passes the non-SQL filters (currently
// just the interactive/exec mode gate). Memoized by the caller to read each
// transcript header at most once per search.
func keepSession(f store.Filter, r store.Row, cache map[string]bool) bool {
	if !f.ExcludeExec {
		return true
	}
	key := r.Source + "\x00" + r.SessionID
	if v, ok := cache[key]; ok {
		return v
	}
	keep := adapter.Mode(r.Path, r.Source) != "exec"
	cache[key] = keep
	return keep
}

type Mode string

const (
	ModeHybrid   Mode = "hybrid"
	ModeSemantic Mode = "semantic"
	ModeLexical  Mode = "lexical"
)

// Result is one session hit for display.
type Result struct {
	Source    string  `json:"source"`
	SessionID string  `json:"session_id"`
	Project   string  `json:"project"`
	Title     string  `json:"title"`
	Role      string  `json:"role"`
	Ts        int64   `json:"ts"`
	Path      string  `json:"path"` // transcript file on disk
	Score     float64 `json:"score"`
	Snippet   string  `json:"snippet"`
}

// Reranker reorders candidate rows for a query. Identity{} is a no-op.
type Reranker interface {
	Rerank(query string, rows []store.Row) ([]store.Row, []float64, error)
}

type Identity struct{}

func (Identity) Rerank(_ string, rows []store.Row) ([]store.Row, []float64, error) {
	s := make([]float64, len(rows))
	for i := range s {
		s[i] = float64(len(rows) - i) // preserve incoming order
	}
	return rows, s, nil
}

const rrfK = 60.0

// Run executes a search and returns up to limit session-level results.
func Run(st *store.Store, em *embed.Client, query string, mode Mode, f store.Filter, limit int, rr Reranker) ([]Result, error) {
	if rr == nil {
		rr = Identity{}
	}
	rowByID := map[int64]store.Row{}
	cosByID := map[int64]float64{}
	var semOrder, lexOrder []int64

	if mode == ModeHybrid || mode == ModeSemantic {
		// Only rank chunks embedded with the query model — vectors from other
		// models occupy a different space, so cosine against them is noise.
		// Lexical (below) is model-agnostic and still sees every chunk.
		cands, err := st.Candidates(f, em.Model)
		if err != nil {
			return nil, err
		}
		qv, err := em.EmbedQuery(query)
		if err != nil {
			return nil, err
		}
		type sc struct {
			id int64
			s  float32
		}
		scored := make([]sc, 0, len(cands))
		for _, r := range cands {
			rowByID[r.ID] = r
			c := cosine(qv, r.Embedding)
			cosByID[r.ID] = float64(c)
			scored = append(scored, sc{r.ID, c})
		}
		sort.Slice(scored, func(i, j int) bool { return scored[i].s > scored[j].s })
		for _, x := range scored {
			semOrder = append(semOrder, x.id)
		}
	}

	if mode == ModeHybrid || mode == ModeLexical {
		lex, err := st.Lexical(buildMatch(query), f, 300)
		if err != nil {
			return nil, err
		}
		for _, r := range lex {
			if _, ok := rowByID[r.ID]; !ok {
				rowByID[r.ID] = r
			}
			lexOrder = append(lexOrder, r.ID)
		}
	}

	// Rank chunk IDs by the chosen strategy.
	var order []int64
	score := map[int64]float64{}
	switch mode {
	case ModeSemantic:
		order = semOrder
		score = cosByID // real cosine similarity
	case ModeLexical:
		order = lexOrder
		for i, id := range order {
			score[id] = 1.0 / float64(i+1)
		}
	default: // hybrid
		score = rrf(semOrder, lexOrder, rrfK)
		order = sortedByScore(score)
	}

	// Collapse to best chunk per session, capped to a rerank window. Mode
	// exclusion happens here — before the window/limit — so excluding exec
	// sessions never shrinks the result count below what's available.
	const rerankWindow = 50
	seen := map[string]bool{}
	modeCache := map[string]bool{}
	var top []store.Row
	for _, id := range order {
		r := rowByID[id]
		key := r.Source + "\x00" + r.SessionID
		if seen[key] {
			continue
		}
		seen[key] = true
		if !keepSession(f, r, modeCache) {
			continue
		}
		top = append(top, r)
		if len(top) >= rerankWindow {
			break
		}
	}

	// Optional reranking pass over the session-level window.
	ranked, rrScores, err := rr.Rerank(query, top)
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, limit)
	for i, r := range ranked {
		sc := rrScores[i]
		if _, ok := rr.(Identity); ok {
			sc = score[r.ID] // expose the real retrieval score, not the placeholder
		}
		results = append(results, Result{
			Source: r.Source, SessionID: r.SessionID, Project: r.Project,
			Title: r.Title, Role: r.Role, Ts: r.Ts, Path: r.Path, Score: sc,
			Snippet: snippet(r.Text, 160),
		})
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

// RunAll finds sessions where EVERY keyword in the query appears somewhere in
// the session (not necessarily the same chunk). It intersects the per-term
// session sets and ranks survivors by summed match strength. Pure lexical —
// no embeddings, so it works even if Ollama is down.
func RunAll(st *store.Store, query string, f store.Filter, limit int) ([]Result, error) {
	terms := splitTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}

	perTerm := make([]map[string]store.SessHit, len(terms))
	for i, t := range terms {
		m, err := st.SessionsMatching(`"`+t+`"`, f)
		if err != nil {
			return nil, err
		}
		if len(m) == 0 {
			return nil, nil // a term nobody has → empty intersection
		}
		perTerm[i] = m
	}

	// Intersect session keys across all terms.
	inter := map[string]bool{}
	for k := range perTerm[0] {
		inter[k] = true
	}
	for _, m := range perTerm[1:] {
		for k := range inter {
			if _, ok := m[k]; !ok {
				delete(inter, k)
			}
		}
	}

	type agg struct {
		total float64
		row   store.Row
	}
	var list []agg
	for k := range inter {
		var total, bestScore float64
		var best store.Row
		first := true
		for _, m := range perTerm {
			h := m[k]
			total += h.Score
			if first || h.Score > bestScore {
				best, bestScore, first = h.Row, h.Score, false
			}
		}
		list = append(list, agg{total, best})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].total != list[j].total {
			return list[i].total > list[j].total
		}
		return list[i].row.Ts > list[j].row.Ts
	})

	results := make([]Result, 0, limit)
	modeCache := map[string]bool{}
	for _, a := range list {
		r := a.row
		if !keepSession(f, r, modeCache) {
			continue
		}
		results = append(results, Result{
			Source: r.Source, SessionID: r.SessionID, Project: r.Project,
			Title: r.Title, Role: r.Role, Ts: r.Ts, Path: r.Path, Score: a.total,
			Snippet: snippet(r.Text, 160),
		})
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

// splitTerms extracts alphanumeric keywords from a free-text query.
func splitTerms(q string) []string {
	var terms []string
	for _, w := range strings.Fields(q) {
		w = strings.TrimFunc(w, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if w != "" {
			terms = append(terms, w)
		}
	}
	return terms
}

func rrf(a, b []int64, k float64) map[int64]float64 {
	out := map[int64]float64{}
	for rank, id := range a {
		out[id] += 1.0 / (k + float64(rank+1))
	}
	for rank, id := range b {
		out[id] += 1.0 / (k + float64(rank+1))
	}
	return out
}

func sortedByScore(m map[int64]float64) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if m[ids[i]] != m[ids[j]] {
			return m[ids[i]] > m[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// buildMatch turns a free-text query into a lenient FTS5 MATCH expression:
// each alphanumeric term quoted, OR-joined.
func buildMatch(q string) string {
	terms := splitTerms(q)
	if len(terms) == 0 {
		return `""`
	}
	for i, t := range terms {
		terms[i] = `"` + t + `"`
	}
	return strings.Join(terms, " OR ")
}

func snippet(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return string(r)
}
