package index

import (
	"context"

	"github.com/ramsrib/recall-cli/internal/adapter"
	"github.com/ramsrib/recall-cli/internal/store"
)

// RefreshSessions reconciles the `sessions` registry with disk: it discovers all
// sessions, re-reads metadata for new or changed sessions, and prunes rows for
// sessions that are gone. Unchanged JSONL files cost only a stat. `recall list` reads
// this registry, so new sessions appear without an embedding reindex. It stops
// early, returning ctx.Err(), if ctx is cancelled.
func RefreshSessions(ctx context.Context, st *store.Store) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	files, closeSnapshots, err := adapter.Discover()
	if err != nil {
		return err
	}
	defer closeSnapshots()
	manifest, err := st.SessionsManifest()
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(files))
	for _, sf := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		present[sf.Path] = true
		mt, sz, lastTs, err := sf.Stamp()
		if err != nil {
			continue
		}
		if m, ok := manifest[sf.Path]; ok && m.MtimeNs == mt && m.Size == sz {
			continue // unchanged — cache hit, no open
		}
		info, err := adapter.ReadInfo(sf)
		if err != nil {
			continue
		}
		st.UpsertSession(store.SessionRow{
			Path: sf.Path, Source: sf.Source, SessionID: info.SessionID,
			Project: info.Project, Title: info.Title,
			FirstTs: info.FirstTs, LastTs: lastTs, MsgCount: info.MsgCount,
			Mode: info.Mode, MtimeNs: mt, Size: sz,
		})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return st.PruneSessions(present)
}
