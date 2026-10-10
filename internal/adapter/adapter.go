// Package adapter discovers Claude Code and Codex session files and normalizes
// their JSONL records into a single Record shape.
package adapter

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
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
	// PrefixHash is a SHA-256 of bytes [0, Offset). If the file no longer
	// starts with them, it was rewritten rather than appended to.
	PrefixHash string `json:"prefix_hash"`

	// Session context a tail parse cannot see. Codex states the session id,
	// cwd and start time once, in the session_meta line at the top; the title
	// comes from an ai-title line (Claude) or the first real user message (Codex).
	SessionID string `json:"session_id,omitempty"`
	Project   string `json:"project,omitempty"`
	Ts        int64  `json:"ts,omitempty"`
	Title     string `json:"title,omitempty"`
}

// ErrRewritten means a file no longer starts with what its cursor consumed, so
// it has to be parsed from the start.
var ErrRewritten = errors.New("transcript was rewritten, not appended to")

// ParseFrom parses the records appended after cur (from the start for a zero
// Cursor) and returns the cursor for the next pass. Every returned record
// carries next.Title, which may differ from cur.Title when the tail renamed
// the session. It returns ErrRewritten if the file no longer begins with the
// bytes cur consumed. Checking that costs one sequential read of the prefix,
// far less than parsing it.
func ParseFrom(sf SessionFile, cur Cursor) (recs []Record, next Cursor, err error) {
	f, err := os.Open(sf.Path)
	if err != nil {
		return nil, cur, err
	}
	defer f.Close()
	h := sha256.New()
	if cur.Offset > 0 {
		if _, err := io.CopyN(h, f, cur.Offset); err != nil || hex.EncodeToString(h.Sum(nil)) != cur.PrefixHash {
			return nil, cur, ErrRewritten
		}
	}
	ls := &lines{r: bufio.NewReaderSize(f, 1<<20), h: h, end: cur.Offset}
	switch sf.Source {
	case "claude":
		recs, next, err = parseClaude(ls, cur)
	case "codex":
		recs, next, err = parseCodex(ls, cur)
	default:
		return nil, cur, fmt.Errorf("unknown source %q", sf.Source)
	}
	if err != nil {
		return nil, cur, err
	}
	next.Offset, next.PrefixHash = ls.end, hex.EncodeToString(h.Sum(nil))
	return recs, next, nil
}

// lines reads a transcript onward from a cursor's offset, adding every byte it
// consumes to the prefix hash.
type lines struct {
	r   *bufio.Reader
	h   hash.Hash
	end int64 // offset just past the last consumed line
}

// each calls fn with every complete line left in the file.
func (l *lines) each(fn func(line []byte)) error {
	for {
		line, err := l.r.ReadBytes('\n')
		if len(line) > 0 {
			// An unterminated last line may be a record still being written.
			// Take it only once it parses; otherwise leave it for the next pass.
			if line[len(line)-1] != '\n' && !json.Valid(line) {
				return nil
			}
			l.h.Write(line)
			l.end += int64(len(line))
			fn(bytes.TrimRight(line, "\r\n"))
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
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
