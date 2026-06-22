package store

import (
	"database/sql"
	"strings"
)

// SessionMeta is one row in the session list, read from the sessions registry
// (one row per session file) joined with pin and summary state. This is what
// powers the app's manager list (and `recall list`); it no longer aggregates
// the chunk search index.
type SessionMeta struct {
	Source     string `json:"source"`
	SessionID  string `json:"session_id"`
	Project    string `json:"project"`
	Title      string `json:"title"`
	FirstTs    int64  `json:"first_ts"`
	LastTs     int64  `json:"last_ts"`
	MsgCount   int    `json:"msg_count"`
	Path       string `json:"path"`
	Pinned     bool   `json:"pinned"`
	PinnedAt   int64  `json:"pinned_at,omitempty"`
	HasSummary bool   `json:"has_summary"`
	Archived   bool   `json:"archived"`
	// Mode is "interactive" (a human-driven TUI/desktop session) or "exec" (a
	// non-interactive, programmatic run — `codex exec`, `claude -p`/SDK).
	Mode string `json:"mode"`
}

// SessionRow is a persisted registry row (one per session file). mtime_ns +
// size are the cache key the refresh pass uses to skip unchanged files.
type SessionRow struct {
	Path, Source, SessionID, Project, Title, Mode string
	FirstTs, LastTs                               int64
	MsgCount                                      int
	MtimeNs, Size                                 int64
}

// FileStamp is the on-disk fingerprint stored per session, for cache validation.
type FileStamp struct{ MtimeNs, Size int64 }

// SessionsManifest returns the cached fingerprint of every known session file,
// so the refresh pass can stat-and-skip files that haven't changed.
func (s *Store) SessionsManifest() (map[string]FileStamp, error) {
	rows, err := s.db.Query(`SELECT path, mtime_ns, size FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileStamp{}
	for rows.Next() {
		var p string
		var st FileStamp
		if err := rows.Scan(&p, &st.MtimeNs, &st.Size); err != nil {
			return nil, err
		}
		out[p] = st
	}
	return out, rows.Err()
}

// UpsertSession writes (or replaces) one session's registry row.
func (s *Store) UpsertSession(r SessionRow) error {
	_, err := s.db.Exec(`INSERT INTO sessions
		(path, source, session_id, project, title, first_ts, last_ts, msg_count, mode, mtime_ns, size)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			source=excluded.source, session_id=excluded.session_id,
			project=excluded.project, title=excluded.title,
			first_ts=excluded.first_ts, last_ts=excluded.last_ts,
			msg_count=excluded.msg_count, mode=excluded.mode,
			mtime_ns=excluded.mtime_ns, size=excluded.size`,
		r.Path, r.Source, r.SessionID, r.Project, r.Title,
		r.FirstTs, r.LastTs, r.MsgCount, r.Mode, r.MtimeNs, r.Size)
	return err
}

// PruneSessions deletes registry rows whose path is no longer present on disk.
func (s *Store) PruneSessions(present map[string]bool) error {
	rows, err := s.db.Query(`SELECT path FROM sessions`)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		if !present[p] {
			gone = append(gone, p)
		}
	}
	rows.Close()
	for _, p := range gone {
		if _, err := s.db.Exec(`DELETE FROM sessions WHERE path=?`, p); err != nil {
			return err
		}
	}
	return nil
}

// ListSessions returns one entry per session, newest-activity first, from the
// sessions registry joined with pin and summary state.
func (s *Store) ListSessions(f Filter) ([]SessionMeta, error) {
	var conds []string
	var args []any
	if f.Source != "" {
		conds = append(conds, "m.source=?")
		args = append(args, f.Source)
	}
	if f.Project != "" {
		conds = append(conds, "m.project LIKE ?")
		args = append(args, "%"+f.Project+"%")
	}
	if f.Since > 0 {
		conds = append(conds, "m.last_ts>=?")
		args = append(args, f.Since)
	}
	where := ""
	if len(conds) > 0 {
		where = " AND " + strings.Join(conds, " AND ")
	}
	q := `SELECT m.source, m.session_id, COALESCE(m.project,''), COALESCE(m.title,''),
			m.first_ts, m.last_ts, m.msg_count, m.path, COALESCE(m.mode,''),
			CASE WHEN p.session_id IS NOT NULL THEN 1 ELSE 0 END,
			COALESCE(p.pinned_at,0),
			CASE WHEN sm.session_id IS NOT NULL THEN 1 ELSE 0 END
		FROM sessions m
		LEFT JOIN pins p       ON p.source=m.source  AND p.session_id=m.session_id
		LEFT JOIN summaries sm ON sm.source=m.source AND sm.session_id=m.session_id
		WHERE 1=1` + where + `
		ORDER BY m.last_ts DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionMeta
	for rows.Next() {
		var m SessionMeta
		var pinned, hasSummary int
		if err := rows.Scan(&m.Source, &m.SessionID, &m.Project, &m.Title,
			&m.FirstTs, &m.LastTs, &m.MsgCount, &m.Path, &m.Mode,
			&pinned, &m.PinnedAt, &hasSummary); err != nil {
			return nil, err
		}
		m.Pinned = pinned == 1
		m.HasSummary = hasSummary == 1
		// Codex archives live under ~/.codex/archived_sessions/. Claude has no
		// archive concept at the tool level.
		m.Archived = m.Source == "codex" && strings.Contains(m.Path, "/archived_sessions/")
		out = append(out, m)
	}
	return out, rows.Err()
}

// SessionByID resolves a session_id to its source, project (cwd), and transcript
// path from the registry. session_ids are UUIDs, unique across both tools.
func (s *Store) SessionByID(sessionID string) (source, project, path string, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT source, COALESCE(project,''), path FROM sessions WHERE session_id=? LIMIT 1`,
		sessionID).Scan(&source, &project, &path)
	if err == sql.ErrNoRows {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	return source, project, path, true, nil
}

// --- pins -------------------------------------------------------------------

func (s *Store) Pin(source, sessionID string, at int64) error {
	_, err := s.db.Exec(`INSERT INTO pins(source, session_id, pinned_at) VALUES(?,?,?)
		ON CONFLICT(source, session_id) DO UPDATE SET pinned_at=excluded.pinned_at`,
		source, sessionID, at)
	return err
}

func (s *Store) Unpin(source, sessionID string) error {
	_, err := s.db.Exec(`DELETE FROM pins WHERE source=? AND session_id=?`, source, sessionID)
	return err
}

func (s *Store) IsPinned(source, sessionID string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM pins WHERE source=? AND session_id=?`, source, sessionID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// --- summaries --------------------------------------------------------------

// GetSummary returns a cached summary if present and (when mtimeNs>0) still
// fresh for the current transcript mtime. The bool is false on a miss/stale.
func (s *Store) GetSummary(source, sessionID string, mtimeNs int64) (string, bool, error) {
	var summary string
	var cachedMtime int64
	err := s.db.QueryRow(`SELECT summary, file_mtime_ns FROM summaries WHERE source=? AND session_id=?`,
		source, sessionID).Scan(&summary, &cachedMtime)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if mtimeNs > 0 && cachedMtime != mtimeNs {
		return "", false, nil // transcript changed since this summary
	}
	return summary, true, nil
}

func (s *Store) SetSummary(source, sessionID string, mtimeNs int64, model, summary string, createdAt int64) error {
	_, err := s.db.Exec(`INSERT INTO summaries(source, session_id, file_mtime_ns, model, summary, created_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(source, session_id) DO UPDATE SET
			file_mtime_ns=excluded.file_mtime_ns, model=excluded.model,
			summary=excluded.summary, created_at=excluded.created_at`,
		source, sessionID, mtimeNs, model, summary, createdAt)
	return err
}
