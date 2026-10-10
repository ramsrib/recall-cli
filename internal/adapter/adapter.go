// Package adapter discovers Claude Code and Codex session files and normalizes
// their JSONL records into a single Record shape.
package adapter

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is one normalized message from either tool.
type Record struct {
	Source    string // "claude" | "codex"
	SessionID string
	Project   string // working directory the session ran in
	Title     string // session title (whole-session)
	Timestamp int64  // unix seconds
	Role      string // "user" | "assistant"
	Text      string
}

// SessionFile is a discovered transcript on disk.
type SessionFile struct {
	Path   string
	Source string
}

// Discover walks both tools' session directories and returns every transcript.
func Discover() ([]SessionFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var out []SessionFile

	claudeRoot := filepath.Join(home, ".claude", "projects")
	_ = filepath.WalkDir(claudeRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Sub-agent transcripts live under `<session-id>/subagents/agent-*.jsonl`
		// and carry the PARENT session's id. Listing them produces duplicate
		// session rows (one per sub-agent), which is noise and breaks the app's
		// list (duplicate identities → blank gaps). They belong to the parent
		// session, so skip the whole subtree.
		if d.IsDir() && d.Name() == "subagents" {
			return fs.SkipDir
		}
		// A Claude transcript is `<session-uuid>.jsonl`. Third-party tools may drop
		// sidecar files beside it that share the session's basename and add their
		// own suffix (`<session-uuid>.<tool>.jsonl`) — side-channel logs, not
		// transcripts. They parse to nothing and would create bogus session rows, so
		// skip them. Extend the guard below for any other sidecar suffix you hit.
		if !d.IsDir() && strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".mentes.jsonl") {
			out = append(out, SessionFile{Path: path, Source: "claude"})
		}
		return nil
	})

	for _, root := range []string{
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".codex", "archived_sessions"),
	} {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(path, ".jsonl") {
				out = append(out, SessionFile{Path: path, Source: "codex"})
			}
			return nil
		})
	}
	return out, nil
}

// Parse reads a whole session file.
func Parse(sf SessionFile) ([]Record, error) {
	recs, _, err := ParseFrom(sf, Cursor{})
	return recs, err
}

// Cursor marks where a previous parse of a file stopped, so the next pass can
// read only what was appended since. Transcripts are append-only logs, and a
// live session grows by a few messages between passes while its file can be
// tens of MB; re-reading (and re-embedding) the whole thing each time is the
// cost this avoids.
type Cursor struct {
	Offset int64 `json:"offset"` // bytes consumed: always just past a complete line
	// TailHash is a hash of the bytes just before Offset. If they no longer
	// match, the file was rewritten rather than appended to, and the caller
	// must re-parse from the start.
	TailHash string `json:"tail_hash"`

	// Session context a tail parse cannot see. Codex states the session id,
	// cwd and start time once, in the session_meta line at the top; the title
	// comes from an ai-title line (Claude) or the first real user message (Codex).
	SessionID string `json:"session_id,omitempty"`
	Project   string `json:"project,omitempty"`
	Ts        int64  `json:"ts,omitempty"`
	Title     string `json:"title,omitempty"`
}

// ParseFrom parses the records appended after cur (from the start for a zero
// Cursor) and returns the cursor for the next pass. Every returned record
// carries next.Title, which may differ from cur.Title when the tail renamed
// the session.
func ParseFrom(sf SessionFile, cur Cursor) (recs []Record, next Cursor, err error) {
	switch sf.Source {
	case "claude":
		recs, next, err = parseClaude(sf.Path, cur)
	case "codex":
		recs, next, err = parseCodex(sf.Path, cur)
	default:
		return nil, cur, fmt.Errorf("unknown source %q", sf.Source)
	}
	if err != nil {
		return nil, cur, err
	}
	if next.TailHash, err = tailHash(sf.Path, next.Offset); err != nil {
		return nil, cur, err
	}
	return recs, next, nil
}

// Resumable reports whether path still begins with the bytes cur consumed, so
// a pass can parse from cur.Offset instead of from the start.
func (cur Cursor) Resumable(path string, size int64) bool {
	if cur.Offset <= 0 || cur.TailHash == "" || size < cur.Offset {
		return false
	}
	h, err := tailHash(path, cur.Offset)
	return err == nil && h == cur.TailHash
}

// tailWindow is how many bytes before a cursor's offset its hash covers.
const tailWindow = 4096

func tailHash(path string, offset int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	start := max(offset-tailWindow, 0)
	buf := make([]byte, offset-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// scanLines calls fn with each complete line of path from byte offset from
// onward, and returns the offset just past the last line it consumed.
func scanLines(path string, from int64, fn func(line []byte)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return from, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return from, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	end := from
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			// An unterminated last line may be a record still being written.
			// Take it only once it parses; otherwise leave it for the next pass.
			if line[len(line)-1] != '\n' && !json.Valid(line) {
				return end, nil
			}
			end += int64(len(line))
			fn(bytes.TrimRight(line, "\r\n"))
		}
		if err == io.EOF {
			return end, nil
		}
		if err != nil {
			return end, err
		}
	}
}

// parseISO parses an RFC3339(/Nano) timestamp to unix seconds; 0 on failure.
func parseISO(s string) int64 {
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Unix()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}

// firstLine returns the first line of s, trimmed and capped at max runes.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return string(r)
}
