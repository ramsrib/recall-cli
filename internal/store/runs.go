package store

import (
	"database/sql"
	"encoding/json"
)

// IndexRun is one recorded `recall index`. Files holds the costliest files it
// indexed, slowest first, so a run that took long says why.
type IndexRun struct {
	StartedAt   int64     `json:"started_at"`
	DurationMs  int64     `json:"duration_ms"`
	EmbedMs     int64     `json:"embed_ms"`
	Status      string    `json:"status"` // ok | error | interrupted | locked
	Error       string    `json:"error,omitempty"`
	Model       string    `json:"model"`
	Host        string    `json:"host"`
	Interactive bool      `json:"interactive"`
	New         int       `json:"new"`
	Appended    int       `json:"appended"`
	Changed     int       `json:"changed"`
	Remodeled   int       `json:"remodeled"`
	Skipped     int       `json:"skipped"`
	Deleted     int       `json:"deleted"`
	Chunks      int       `json:"chunks"`
	Files       []RunFile `json:"files,omitempty"`
}

// RunFile is what indexing one file cost within a run.
type RunFile struct {
	Path    string `json:"path"`
	How     string `json:"how"` // new | appended | changed | remodeled
	Chunks  int    `json:"chunks"`
	Ms      int64  `json:"ms"`
	EmbedMs int64  `json:"embed_ms"`
}

// LogIndexRun records one index run.
func (s *Store) LogIndexRun(r IndexRun) error {
	files, _ := json.Marshal(r.Files)
	_, err := s.db.Exec(`INSERT INTO index_runs
		(started_at, duration_ms, embed_ms, status, error, model, host, interactive,
		 n_new, n_appended, n_changed, n_remodeled, n_skipped, n_deleted, chunks, files)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.StartedAt, r.DurationMs, r.EmbedMs, r.Status, r.Error, r.Model, r.Host, r.Interactive,
		r.New, r.Appended, r.Changed, r.Remodeled, r.Skipped, r.Deleted, r.Chunks, string(files))
	return err
}

// RecentIndexRuns returns the most recent index runs, newest first.
func (s *Store) RecentIndexRuns(limit int) ([]IndexRun, error) {
	rows, err := s.db.Query(`SELECT started_at, duration_ms, embed_ms, status, COALESCE(error,''),
		model, host, interactive, n_new, n_appended, n_changed, n_remodeled, n_skipped, n_deleted,
		chunks, COALESCE(files,'')
		FROM index_runs ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexRun
	for rows.Next() {
		var r IndexRun
		var files sql.NullString
		if err := rows.Scan(&r.StartedAt, &r.DurationMs, &r.EmbedMs, &r.Status, &r.Error,
			&r.Model, &r.Host, &r.Interactive, &r.New, &r.Appended, &r.Changed, &r.Remodeled,
			&r.Skipped, &r.Deleted, &r.Chunks, &files); err != nil {
			return nil, err
		}
		if files.String != "" {
			_ = json.Unmarshal([]byte(files.String), &r.Files)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
