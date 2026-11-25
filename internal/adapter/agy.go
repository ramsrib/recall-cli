package adapter

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const Agy = "agy"
const agyDatabase = "conversation_summaries.db"

type agySession struct {
	db       *sql.DB
	info     Info
	revision int64
	lastNs   int64
	hasText  bool
	history  []agyPrompt
}

type agyPrompt struct {
	Display        string `json:"display"`
	Timestamp      int64  `json:"timestamp"` // Unix milliseconds
	Workspace      string `json:"workspace"`
	ConversationID string `json:"conversationId"`
	Type           string `json:"type"`
}

func discoverAgy(home string) ([]SessionFile, func(), error) {
	var files []SessionFile
	var cleanups []func()
	closeAll := func() {
		for _, closeSnapshot := range cleanups {
			closeSnapshot()
		}
	}
	byID := make(map[string]int)
	for _, name := range []string{"antigravity-cli", "antigravity"} {
		path := filepath.Join(home, ".gemini", name, agyDatabase)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		sessions, closeSnapshot, err := readAgy(path, "")
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("read Antigravity sessions at %s: %w", path, err)
		}
		cleanups = append(cleanups, closeSnapshot)
		for _, sf := range sessions {
			id := sf.agy.info.SessionID
			if i, ok := byID[id]; ok {
				// The desktop can mirror a CLI summary without its FTS text.
				// Prefer searchable copies, then the newest copy. Ties keep CLI.
				old := files[i].agy
				if (sf.agy.hasText && !old.hasText) ||
					(sf.agy.hasText == old.hasText && sf.agy.lastNs > old.lastNs) {
					files[i] = sf
				}
				continue
			}
			byID[id] = len(files)
			files = append(files, sf)
		}
	}
	return files, closeAll, nil
}

// OpenAgySession reopens a locator from the sessions registry. Its stamp and
// records come from the same snapshot so summaries can cache the text they read.
func OpenAgySession(locator string) (SessionFile, func(), error) {
	i := strings.LastIndexByte(locator, '#')
	if i < 0 || i == len(locator)-1 || filepath.Base(locator[:i]) != agyDatabase {
		return SessionFile{}, nil, fmt.Errorf("invalid Antigravity session locator %q", locator)
	}
	files, closeSnapshot, err := readAgy(locator[:i], locator[i+1:])
	if err != nil {
		return SessionFile{}, nil, err
	}
	if len(files) == 0 {
		closeSnapshot()
		return SessionFile{}, nil, fmt.Errorf("Antigravity conversation %q no longer exists", locator[i+1:])
	}
	return files[0], closeSnapshot, nil
}

func readAgy(path, id string) ([]SessionFile, func(), error) {
	db, closeSnapshot, err := snapshotAgy(path)
	if err != nil {
		return nil, nil, err
	}
	files, err := agySessions(db, path, id)
	if err != nil {
		closeSnapshot()
		return nil, nil, err
	}
	return files, closeSnapshot, nil
}

func agySessions(db *sql.DB, path, id string) ([]SessionFile, error) {
	history, err := readAgyHistory(filepath.Join(filepath.Dir(path), "history.jsonl"), id)
	if err != nil {
		return nil, err
	}
	// Antigravity writes a metadata row (chunk_index = -1) separately from
	// the text chunks. Track both so a summary update and a later FTS update
	// each invalidate just this conversation's index entry.
	rows, err := db.Query(`
		WITH fts AS (
			SELECT conversation_id,
				MAX(CAST(last_modified_time AS INTEGER)) AS modified,
				MAX(CAST(last_step_created_at AS INTEGER)) AS last_step,
				SUM(CASE WHEN user_body <> '' OR agent_body <> '' THEN 1 ELSE 0 END) AS bodies
			FROM conversations_fts
			WHERE (?1 = '' OR conversation_id = ?1)
			GROUP BY conversation_id
		)
		SELECT s.conversation_id, s.title, s.workspace_uris, s.step_count,
			CAST(s.last_modified_time AS TEXT), CAST(s.last_user_input_time AS TEXT),
			COALESCE(fts.modified, 0), COALESCE(fts.last_step, 0), COALESCE(fts.bodies, 0)
		FROM conversation_summaries AS s
		LEFT JOIN fts ON fts.conversation_id = s.conversation_id
		WHERE s.conversation_id <> '' AND (?1 = '' OR s.conversation_id = ?1)
		ORDER BY s.conversation_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []SessionFile
	for rows.Next() {
		var info Info
		var uris, modified, lastUser string
		var ftsModified, ftsLastStep, bodies int64
		if err := rows.Scan(&info.SessionID, &info.Title, &uris, &info.MsgCount, &modified,
			&lastUser, &ftsModified, &ftsLastStep, &bodies); err != nil {
			return nil, err
		}
		info.Mode = "interactive"
		info.Project = agyWorkspace(uris)
		lastNs := max(agyTime(modified), agyTime(lastUser), ftsModified, ftsLastStep)
		prompts := history[info.SessionID]
		var firstPrompt, lastPrompt int64
		for _, p := range prompts {
			if p.Timestamp > 0 && (firstPrompt == 0 || p.Timestamp < firstPrompt) {
				firstPrompt = p.Timestamp
			}
			lastPrompt = max(lastPrompt, p.Timestamp)
			if info.Project == "" && filepath.IsAbs(p.Workspace) {
				info.Project = p.Workspace
			}
		}
		if lastPrompt > 0 {
			lastNs = max(lastNs, time.UnixMilli(lastPrompt).UnixNano())
		}
		info.LastTs = lastNs / int64(time.Second)
		info.FirstTs = firstPrompt / 1000
		if info.FirstTs == 0 {
			info.FirstTs = agyTime(lastUser) / int64(time.Second)
		}
		if info.FirstTs == 0 {
			info.FirstTs = info.LastTs
		}
		if info.Title == "" && len(prompts) > 0 {
			info.Title = firstLine(prompts[0].Display, 80)
		}

		// A per-conversation revision also invalidates cached summaries. The
		// shared database mtime would invalidate every conversation at once.
		h := sha256.New()
		err := json.NewEncoder(h).Encode([]any{
			info, modified, lastUser, ftsModified, ftsLastStep, bodies, prompts,
		})
		if err != nil {
			return nil, err
		}
		revision := int64(binary.LittleEndian.Uint64(h.Sum(nil))>>1) | 1
		files = append(files, SessionFile{
			Path: path + "#" + info.SessionID, Source: Agy,
			agy: &agySession{
				db: db, info: info, revision: revision, lastNs: lastNs,
				hasText: bodies > 0 || len(prompts) > 0, history: prompts,
			},
		})
	}
	return files, rows.Err()
}

func (s *agySession) records() ([]Record, error) {
	rows, err := s.db.Query(`
		SELECT COALESCE(user_body, ''), COALESCE(agent_body, '')
		FROM conversations_fts WHERE conversation_id = ?
		ORDER BY CAST(chunk_index AS INTEGER), rowid`, s.info.SessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []Record
	add := func(role, text string, ts int64) {
		if strings.TrimSpace(text) != "" {
			records = append(records, Record{
				Source: Agy, SessionID: s.info.SessionID, Project: s.info.Project,
				Title: s.info.Title, Timestamp: ts, Role: role, Text: text,
			})
		}
	}
	hasUser := false
	for rows.Next() {
		var user, agent string
		if err := rows.Scan(&user, &agent); err != nil {
			return nil, err
		}
		hasUser = hasUser || strings.TrimSpace(user) != ""
		add("user", user, s.info.LastTs)
		add("assistant", agent, s.info.LastTs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !hasUser && len(s.history) > 0 {
		agentRecords := records
		records = nil
		for _, p := range s.history {
			ts := p.Timestamp / 1000
			if ts == 0 {
				ts = s.info.LastTs
			}
			add("user", p.Display, ts)
		}
		records = append(records, agentRecords...)
	}
	if s.info.Title == "" {
		title := firstUserTitle(records)
		for i := range records {
			records[i].Title = title
		}
	}
	return records, nil
}

func readAgyHistory(path, id string) (map[string][]agyPrompt, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	history := make(map[string][]agyPrompt)
	lines := &lines{r: bufio.NewReader(f), h: sha256.New()}
	err = lines.each(func(line []byte) {
		var p agyPrompt
		if json.Unmarshal(line, &p) == nil && p.ConversationID != "" &&
			(id == "" || p.ConversationID == id) && p.Type != "slash_command" &&
			strings.TrimSpace(p.Display) != "" {
			history[p.ConversationID] = append(history[p.ConversationID], p)
		}
	})
	return history, err
}

func agyWorkspace(raw string) string {
	var uris []string
	if json.Unmarshal([]byte(raw), &uris) != nil {
		return ""
	}
	for _, uri := range uris {
		u, err := url.Parse(uri)
		if err == nil && u.Scheme == "file" && (u.Host == "" || u.Host == "localhost") &&
			filepath.IsAbs(u.Path) {
			return u.Path
		}
	}
	return ""
}

// SQLite stores summary timestamps as datetime text and FTS timestamps as Unix
// nanoseconds. Treat the zero dates used for empty sessions as unknown.
func agyTime(raw string) int64 {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", time.DateTime} {
		if t, err := time.Parse(layout, raw); err == nil && t.After(time.Unix(0, 0)) {
			return t.UnixNano()
		}
	}
	return 0
}

func snapshotAgy(path string) (*sql.DB, func(), error) {
	dir, err := os.MkdirTemp("", "recall-agy-")
	if err != nil {
		return nil, nil, err
	}
	copyPath := filepath.Join(dir, agyDatabase)
	suffixes := []string{"", "-wal", "-shm"}
	// A checkpoint, file replacement, or WAL append during a copy can mix
	// generations. Retry if any of the source files changed while copying.
	for range 3 {
		before, err := agyFileStats(path, suffixes)
		if err != nil {
			os.RemoveAll(dir)
			return nil, nil, err
		}
		var copyErr error
		for i, suffix := range suffixes {
			if before[i] != nil {
				copyErr = copyAgyFile(path+suffix, copyPath+suffix)
			} else if err := os.Remove(copyPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				copyErr = err
			}
			if copyErr != nil {
				break
			}
		}
		after, err := agyFileStats(path, suffixes)
		if err == nil && !agyFilesUnchanged(before, after) {
			continue
		}
		if err := errors.Join(copyErr, err); err != nil {
			os.RemoveAll(dir)
			return nil, nil, err
		}
		dsn := url.URL{Scheme: "file", Path: copyPath, RawQuery: "mode=rw"}
		db, err := sql.Open("sqlite", dsn.String())
		if err != nil {
			os.RemoveAll(dir)
			return nil, nil, err
		}
		db.SetMaxOpenConns(1)
		return db, func() {
			db.Close()
			os.RemoveAll(dir)
		}, nil
	}
	os.RemoveAll(dir)
	return nil, nil, fmt.Errorf("Antigravity database changed while taking a snapshot: %s", path)
}

func agyFileStats(path string, suffixes []string) ([]os.FileInfo, error) {
	stats := make([]os.FileInfo, len(suffixes))
	for i, suffix := range suffixes {
		fi, err := os.Stat(path + suffix)
		if i > 0 && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stats[i] = fi
	}
	return stats, nil
}

func agyFilesUnchanged(before, after []os.FileInfo) bool {
	for i, first := range before {
		last := after[i]
		if first == nil && last == nil {
			continue
		}
		if first == nil || last == nil || !os.SameFile(first, last) ||
			first.Size() != last.Size() || !first.ModTime().Equal(last.ModTime()) {
			return false
		}
	}
	return true
}

func copyAgyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}
