// Package adapter discovers Claude Code and Codex session files and normalizes
// their JSONL records into a single Record shape.
package adapter

import (
	"fmt"
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
		if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
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

// Parse dispatches to the source-specific parser.
func Parse(sf SessionFile) ([]Record, error) {
	switch sf.Source {
	case "claude":
		return parseClaude(sf.Path)
	case "codex":
		return parseCodex(sf.Path)
	}
	return nil, fmt.Errorf("unknown source %q", sf.Source)
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
