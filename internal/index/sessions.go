package index

import (
	"os"

	"recall/internal/adapter"
	"recall/internal/store"
)

// RefreshSessions reconciles the `sessions` registry with disk: it discovers all
// session files, re-parses only those that are new or changed (by mtime+size),
// and prunes rows for files that are gone. Cheap to call often (e.g. on app
// focus) — unchanged files cost only a stat, never an open. `recall list` reads
// this registry, so new sessions appear without an embedding reindex.
func RefreshSessions(st *store.Store) error {
	files, err := adapter.Discover()
	if err != nil {
		return err
	}
	manifest, err := st.SessionsManifest()
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(files))
	for _, sf := range files {
		present[sf.Path] = true
		fi, err := os.Stat(sf.Path)
		if err != nil {
			continue
		}
		mt, sz := fi.ModTime().UnixNano(), fi.Size()
		if m, ok := manifest[sf.Path]; ok && m.MtimeNs == mt && m.Size == sz {
			continue // unchanged — cache hit, no open
		}
		info, err := adapter.ReadInfo(sf)
		if err != nil {
			continue
		}
		// File mtime is the most reliable "last activity" across both tools —
		// Codex stamps every record with the session start, so record
		// timestamps can't give last_ts.
		st.UpsertSession(store.SessionRow{
			Path: sf.Path, Source: sf.Source, SessionID: info.SessionID,
			Project: info.Project, Title: info.Title,
			FirstTs: info.FirstTs, LastTs: fi.ModTime().Unix(), MsgCount: info.MsgCount,
			Mode: info.Mode, MtimeNs: mt, Size: sz,
		})
	}
	return st.PruneSessions(present)
}
