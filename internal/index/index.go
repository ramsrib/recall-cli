// Package index performs the incremental, both-source indexing pass.
package index

import (
	"fmt"
	"os"
	"strings"

	"github.com/ramsrib/recall-cli/internal/adapter"
	"github.com/ramsrib/recall-cli/internal/embed"
	"github.com/ramsrib/recall-cli/internal/store"
)

// maxChunkRunes ~ 512 tokens. Both supported models handle ≥8k, so this is comfortably safe.
const maxChunkRunes = 2000

// embedBatch is how many chunks we send to Ollama per request.
const embedBatch = 64

type Stats struct {
	New, Changed, Remodeled, Skipped, Deleted, Chunks int
}

// Run reconciles the index with disk. now is the indexed_at timestamp (unix sec).
//
// Each file records the embedding model that produced its vectors. A file is
// re-embedded when its content changed (mtime/size) OR when it was indexed
// under a different model than the one in use now. The model switch is handled
// per-file rather than as one global wipe, so it is incremental and resumable:
// an interrupted migration just leaves some files on the old model, and the
// query path simply won't rank those until a later pass brings them over.
func Run(st *store.Store, em *embed.Client, now int64, log func(string)) (Stats, error) {
	var stats Stats

	// meta.embed_model tracks the most recently used model — the default the
	// query path embeds with. Per-file embed_model is the source of truth for
	// what actually needs re-embedding.
	prev, _ := st.GetMeta("embed_model")
	if prev != "" && prev != em.Model {
		log(fmt.Sprintf("note: switching embedding model %q → %q; files on the old model will be re-embedded incrementally", prev, em.Model))
	}
	if err := st.SetMeta("embed_model", em.Model); err != nil {
		return stats, err
	}

	files, err := adapter.Discover()
	if err != nil {
		return stats, err
	}
	manifest, err := st.Manifest()
	if err != nil {
		return stats, err
	}

	present := make(map[string]bool, len(files))
	for _, sf := range files {
		present[sf.Path] = true
		fi, err := os.Stat(sf.Path)
		if err != nil {
			continue
		}
		mt, sz := fi.ModTime().UnixNano(), fi.Size()

		m, indexed := manifest[sf.Path]
		contentSame := indexed && m.MtimeNs == mt && m.Size == sz
		modelSame := m.EmbedModel == em.Model
		if contentSame && modelSame {
			stats.Skipped++
			continue
		}

		recs, err := adapter.Parse(sf)
		if err != nil {
			log(fmt.Sprintf("skip %s: %v", sf.Path, err))
			continue
		}
		chunks := chunkRecords(recs)
		if err := embedChunks(em, chunks); err != nil {
			return stats, fmt.Errorf("embed %s: %w", sf.Path, err)
		}
		if err := st.ReplaceFile(sf.Path, sf.Source, em.Model, mt, sz, now, chunks); err != nil {
			return stats, err
		}
		stats.Chunks += len(chunks)
		why := ""
		switch {
		case !indexed:
			stats.New++
		case contentSame: // unchanged content, different model
			stats.Remodeled++
			why = " (re-embed: model change)"
		default:
			stats.Changed++
		}
		log(fmt.Sprintf("  [%s] %s (%d chunks)%s", sf.Source, trimHome(sf.Path), len(chunks), why))
	}

	for path := range manifest {
		if !present[path] {
			if err := st.DeleteFile(path); err != nil {
				return stats, err
			}
			stats.Deleted++
		}
	}

	// Keep the sessions registry (what `recall list` reads) in sync with disk.
	if err := RefreshSessions(st); err != nil {
		log(fmt.Sprintf("session registry refresh failed: %v", err))
	}
	return stats, nil
}

func chunkRecords(recs []adapter.Record) []store.Chunk {
	var out []store.Chunk
	for _, r := range recs {
		for _, piece := range splitText(r.Text, maxChunkRunes) {
			out = append(out, store.Chunk{
				Source: r.Source, SessionID: r.SessionID, Project: r.Project,
				Title: r.Title, Ts: r.Timestamp, Role: r.Role, Text: piece,
			})
		}
	}
	return out
}

func embedChunks(em *embed.Client, chunks []store.Chunk) error {
	for i := 0; i < len(chunks); i += embedBatch {
		end := min(i+embedBatch, len(chunks))
		texts := make([]string, 0, end-i)
		for _, c := range chunks[i:end] {
			texts = append(texts, c.Text)
		}
		vs, err := em.EmbedDocuments(texts)
		if err != nil {
			return err
		}
		for j := range vs {
			chunks[i+j].Embedding = vs[j]
		}
	}
	return nil
}

func splitText(s string, max int) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	r := []rune(s)
	if len(r) <= max {
		return []string{s}
	}
	var out []string
	for i := 0; i < len(r); i += max {
		out = append(out, string(r[i:min(i+max, len(r))]))
	}
	return out
}

func trimHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil {
		if rel := strings.TrimPrefix(p, h); rel != p {
			return "~" + rel
		}
	}
	return p
}
