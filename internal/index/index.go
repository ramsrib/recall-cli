// Package index performs the incremental, both-source indexing pass.
package index

import (
	"encoding/json"
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

// Stats counts files by what a pass did with them. Appended files grew and had
// only their new tail embedded; Changed files were re-read in full.
// fileCursor is what files.cursor holds: the parse cursor plus the mtime of
// the manifest row it was written with. A recall build that predates cursors
// re-indexes a file without touching this column, so a cursor whose mtime no
// longer matches its row is stale and must not be resumed from.
type fileCursor struct {
	adapter.Cursor
	MtimeNs int64 `json:"mtime_ns"`
}

type Stats struct {
	New, Appended, Changed, Remodeled, Skipped, Deleted, Chunks int
}

// Run reconciles the index with disk. now is the indexed_at timestamp (unix sec).
//
// Each file records the embedding model that produced its vectors. A file is
// re-embedded when its content changed (mtime/size) OR when it was indexed
// under a different model than the one in use now. A file that only grew is
// embedded from where the last pass stopped (see adapter.Cursor). The model switch is handled
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

		// A transcript that only grew since the last pass (the common case: a
		// live session) gets just its new tail parsed and embedded. Anything
		// else (a rewrite, a model switch, no cursor yet) re-reads the file.
		var fc fileCursor
		appending := indexed && modelSame && m.Cursor != "" &&
			json.Unmarshal([]byte(m.Cursor), &fc) == nil && fc.MtimeNs == m.MtimeNs &&
			fc.Resumable(sf.Path, sz)
		cur := adapter.Cursor{}
		if appending {
			cur = fc.Cursor
		}
		recs, next, err := adapter.ParseFrom(sf, cur)
		if err != nil {
			log(fmt.Sprintf("skip %s: %v", sf.Path, err))
			continue
		}
		chunks := chunkRecords(recs)
		if err := embedChunks(em, chunks); err != nil {
			return stats, fmt.Errorf("embed %s: %w", sf.Path, err)
		}
		cursor, err := json.Marshal(fileCursor{Cursor: next, MtimeNs: mt})
		if err != nil {
			return stats, err
		}
		u := store.FileUpdate{
			Path: sf.Path, Source: sf.Source, EmbedModel: em.Model,
			MtimeNs: mt, Size: sz, IndexedAt: now, Cursor: string(cursor), Chunks: chunks,
		}
		if appending {
			retitle := ""
			if next.Title != cur.Title {
				retitle = next.Title
			}
			err = st.AppendFile(u, retitle)
		} else {
			err = st.ReplaceFile(u)
		}
		if err != nil {
			return stats, err
		}
		stats.Chunks += len(chunks)
		what := fmt.Sprintf("%d chunks", len(chunks))
		switch {
		case !indexed:
			stats.New++
		case appending:
			stats.Appended++
			what = fmt.Sprintf("+%d chunks appended", len(chunks))
		case contentSame: // unchanged content, different model
			stats.Remodeled++
			what += ", re-embed: model change"
		default:
			stats.Changed++
		}
		log(fmt.Sprintf("  [%s] %s (%s)", sf.Source, trimHome(sf.Path), what))
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
