// Package index performs the incremental, both-source indexing pass.
package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ramsrib/recall-cli/internal/adapter"
	"github.com/ramsrib/recall-cli/internal/embed"
	"github.com/ramsrib/recall-cli/internal/store"
)

// maxChunkRunes ~ 512 tokens. Both supported models handle ≥8k, so this is comfortably safe.
const maxChunkRunes = 2000

// embedBatch is how many chunks we send to Ollama per request.
const embedBatch = 64

// fileCursor is what files.cursor holds: the parse cursor plus the manifest
// row it was written with. A recall build that predates cursors re-indexes a
// file without touching this column, so a cursor whose row has since changed
// is stale and must not be resumed from.
type fileCursor struct {
	adapter.Cursor
	MtimeNs   int64 `json:"mtime_ns"`
	Size      int64 `json:"size"`
	IndexedAt int64 `json:"indexed_at"`
	// Chunks is how many chunks the file had once this cursor was written.
	// It catches the one rewrite the row stamp can't: an old build that
	// stamps the row identically (same second, same stat) but indexed more.
	Chunks int `json:"chunks"`
}

// Stats counts files by what a pass did with them. Appended files grew and had
// only their new tail embedded; Changed files were re-read in full.
type Stats struct {
	New, Appended, Changed, Remodeled, Skipped, Deleted, Chunks int
	// Embed is the time spent waiting on the embedder, the part a remote
	// OLLAMA_HOST moves off this machine.
	Embed time.Duration
	Files []FileStat // every file this pass indexed, in order
}

// FileStat is what indexing one file cost.
type FileStat struct {
	Path, Source string
	How          string // new | appended | changed | remodeled
	Chunks       int
	Took, Embed  time.Duration
}

// Progress receives a pass's events as they happen, all from Run's goroutine.
type Progress interface {
	// Planned reports how many of the files on disk need indexing.
	Planned(todo, total int)
	// Started announces the i-th (1-based) file to index, before it is read.
	Started(i int, path, source string)
	// Embedded reports chunks embedded so far for the current file. The
	// first call, with done == 0, comes once the file is read and chunked.
	Embedded(done, total int)
	// Finished reports the current file as written to the index.
	Finished(i int, f FileStat)
	// Note is anything else worth telling the user.
	Note(msg string)
}

// Run reconciles the index with disk. now is the indexed_at timestamp (unix sec).
//
// Each file records the embedding model that produced its vectors. A file is
// re-embedded when its content changed (mtime/size) OR when it was indexed
// under a different model than the one in use now. A file that only grew is
// embedded from where the last pass stopped (see adapter.Cursor). The model
// switch is handled per-file rather than as one global wipe, so it is
// incremental and resumable: an interrupted migration just leaves some files
// on the old model, and the query path simply won't rank those until a later
// pass brings them over.
//
// Each file is written in its own transaction, so cancelling ctx (or an
// error) keeps every file finished so far; the returned Stats cover them.
func Run(ctx context.Context, st *store.Store, em *embed.Client, now int64, p Progress) (Stats, error) {
	var stats Stats

	// meta.embed_model tracks the most recently used model — the default the
	// query path embeds with. Per-file embed_model is the source of truth for
	// what actually needs re-embedding.
	prev, _ := st.GetMeta("embed_model")
	if prev != "" && prev != em.Model {
		p.Note(fmt.Sprintf("switching embedding model %q → %q; files on the old model will be re-embedded incrementally", prev, em.Model))
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

	// Plan first (a stat per file, nothing opened) so progress can say how
	// much of the pass is left.
	type job struct {
		sf      adapter.SessionFile
		mt, sz  int64
		m       store.FileMeta
		indexed bool
	}
	var todo []job
	present := make(map[string]bool, len(files))
	for _, sf := range files {
		present[sf.Path] = true
		fi, err := os.Stat(sf.Path)
		if err != nil {
			continue
		}
		mt, sz := fi.ModTime().UnixNano(), fi.Size()
		m, indexed := manifest[sf.Path]
		if indexed && m.MtimeNs == mt && m.Size == sz && m.EmbedModel == em.Model {
			stats.Skipped++
			continue
		}
		todo = append(todo, job{sf, mt, sz, m, indexed})
	}
	p.Planned(len(todo), len(files))

	for i, j := range todo {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		p.Started(i+1, j.sf.Path, j.sf.Source)
		f, err := indexFile(ctx, st, em, now, j.sf, j.mt, j.sz, j.m, j.indexed, p)
		// Time spent embedding counts even if the file then didn't make it in.
		stats.Embed += f.Embed
		if errors.Is(err, errSkip) {
			continue
		}
		if err != nil {
			return stats, err
		}
		switch f.How {
		case "new":
			stats.New++
		case "appended":
			stats.Appended++
		case "remodeled":
			stats.Remodeled++
		default:
			stats.Changed++
		}
		stats.Chunks += f.Chunks
		stats.Files = append(stats.Files, f)
		p.Finished(i+1, f)
	}

	for path := range manifest {
		if !present[path] {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			if err := st.DeleteFile(path); err != nil {
				return stats, err
			}
			stats.Deleted++
		}
	}

	// Keep the sessions registry (what `recall list` reads) in sync with disk.
	if err := RefreshSessions(ctx, st); err != nil {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		p.Note(fmt.Sprintf("session registry refresh failed: %v", err))
	}
	return stats, nil
}

// errSkip means a file was passed over this time (already reported through
// Progress.Note); the next pass will retry it.
var errSkip = errors.New("skipped")

// indexFile parses, embeds and stores one file. m and indexed are its
// manifest row as read at the start of the pass.
func indexFile(ctx context.Context, st *store.Store, em *embed.Client, now int64,
	sf adapter.SessionFile, mt, sz int64, m store.FileMeta, indexed bool, p Progress) (FileStat, error) {
	start := time.Now()
	f := FileStat{Path: sf.Path, Source: sf.Source}

	// A transcript that only grew since the last pass (the common case: a
	// live session) gets just its new tail parsed and embedded. Anything else
	// (a rewrite, a model switch, no cursor yet) re-reads the file.
	modelSame := m.EmbedModel == em.Model
	var fc fileCursor
	appending := indexed && modelSame && m.Cursor != "" &&
		json.Unmarshal([]byte(m.Cursor), &fc) == nil &&
		fc.MtimeNs == m.MtimeNs && fc.Size == m.Size && fc.IndexedAt == m.IndexedAt &&
		fc.Chunks == m.Chunks
	cur := adapter.Cursor{}
	if appending {
		cur = fc.Cursor
	}
	recs, next, err := adapter.ParseFrom(sf, cur)
	if errors.Is(err, adapter.ErrRewritten) {
		appending, cur = false, adapter.Cursor{}
		recs, next, err = adapter.ParseFrom(sf, cur)
	}
	if err != nil {
		p.Note(fmt.Sprintf("skip %s: %v", trimHome(sf.Path), err))
		return f, errSkip
	}
	chunks := chunkRecords(recs)
	if f.Embed, err = embedChunks(ctx, em, chunks, p); err != nil {
		if ctx.Err() != nil {
			return f, ctx.Err()
		}
		return f, fmt.Errorf("embed %s: %w", sf.Path, err)
	}
	total := len(chunks)
	if appending {
		total += m.Chunks
	}
	cursor, err := json.Marshal(fileCursor{Cursor: next, MtimeNs: mt, Size: sz, IndexedAt: now, Chunks: total})
	if err != nil {
		return f, err
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
		err = st.AppendFile(u, m, retitle)
	} else {
		err = st.ReplaceFile(u)
	}
	if errors.Is(err, store.ErrStale) {
		p.Note(fmt.Sprintf("skip %s: %v; the next pass picks it up", trimHome(sf.Path), err))
		return f, errSkip
	}
	if err != nil {
		return f, err
	}

	f.Chunks, f.Took = len(chunks), time.Since(start)
	switch {
	case !indexed:
		f.How = "new"
	case appending:
		f.How = "appended"
	case m.MtimeNs == mt && m.Size == sz: // unchanged content, different model
		f.How = "remodeled"
	default:
		f.How = "changed"
	}
	return f, nil
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

// embedChunks fills in each chunk's embedding, reporting progress per batch,
// and returns the time spent waiting on the embedder.
func embedChunks(ctx context.Context, em *embed.Client, chunks []store.Chunk, p Progress) (time.Duration, error) {
	var waited time.Duration
	p.Embedded(0, len(chunks))
	for i := 0; i < len(chunks); i += embedBatch {
		end := min(i+embedBatch, len(chunks))
		texts := make([]string, 0, end-i)
		for _, c := range chunks[i:end] {
			texts = append(texts, c.Text)
		}
		start := time.Now()
		vs, err := em.EmbedDocuments(ctx, texts)
		waited += time.Since(start)
		if err != nil {
			return waited, err
		}
		for j := range vs {
			chunks[i+j].Embedding = vs[j]
		}
		p.Embedded(end, len(chunks))
	}
	return waited, nil
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
