package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func claudeMsg(role, text string) string {
	return `{"type":"` + role + `","sessionId":"s1","cwd":"/p","timestamp":"2026-10-10T00:00:00Z","message":{"role":"` + role + `","content":"` + text + `"}}` + "\n"
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func texts(recs []Record) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Text)
	}
	return out
}

func TestParseFromResumesAppendedClaudeTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	sf := SessionFile{Path: path, Source: "claude"}
	appendFile(t, path, claudeMsg("user", "one")+`{"type":"ai-title","aiTitle":"First"}`+"\n"+claudeMsg("assistant", "two"))

	recs, cur, err := ParseFrom(sf, Cursor{})
	if err != nil || len(recs) != 2 || recs[0].Title != "First" || cur.Offset != size(t, path) {
		t.Fatalf("full parse: recs=%v cur=%+v err=%v", texts(recs), cur, err)
	}

	// The tail renames the session and adds one message; only that message comes back.
	appendFile(t, path, `{"type":"ai-title","aiTitle":"Second"}`+"\n"+claudeMsg("user", "three"))
	recs, next, err := ParseFrom(sf, cur)
	if err != nil || len(recs) != 1 || recs[0].Text != "three" || recs[0].Title != "Second" || next.Title != "Second" {
		t.Fatalf("tail parse: recs=%v next=%+v err=%v", texts(recs), next, err)
	}

	// The cursor a tail parse returns must equal the one a full parse would.
	_, full, _ := ParseFrom(sf, Cursor{})
	if next != full {
		t.Fatalf("tail cursor %+v != full cursor %+v", next, full)
	}
}

func TestParseFromLeavesPartialLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	sf := SessionFile{Path: path, Source: "claude"}
	appendFile(t, path, claudeMsg("user", "one"))
	_, cur, _ := ParseFrom(sf, Cursor{})

	line := claudeMsg("assistant", "two")
	appendFile(t, path, line[:20]) // a record caught mid-write
	recs, next, err := ParseFrom(sf, cur)
	if err != nil || len(recs) != 0 || next.Offset != cur.Offset {
		t.Fatalf("partial line was consumed: recs=%v next=%+v err=%v", texts(recs), next, err)
	}

	appendFile(t, path, line[20:])
	recs, _, err = ParseFrom(sf, next)
	if err != nil || len(recs) != 1 || recs[0].Text != "two" {
		t.Fatalf("completed line: recs=%v err=%v", texts(recs), err)
	}
}

func TestParseFromRejectsRewrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	sf := SessionFile{Path: path, Source: "claude"}
	// Enough after the first message that a rewrite of it is far from the cursor.
	var tail string
	for range 200 {
		tail += claudeMsg("assistant", "filler")
	}
	appendFile(t, path, claudeMsg("user", "one")+tail)
	_, cur, _ := ParseFrom(sf, Cursor{})

	for name, content := range map[string]string{
		"early same-length rewrite": claudeMsg("user", "ONE") + tail + claudeMsg("user", "two"),
		"truncated":                 "",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ParseFrom(sf, cur); err != ErrRewritten {
			t.Fatalf("%s: err=%v, want ErrRewritten", name, err)
		}
	}
}

func TestParseFromKeepsCodexSessionContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	sf := SessionFile{Path: path, Source: "codex"}
	msg := func(role, text string) string {
		return `{"type":"response_item","payload":{"type":"message","role":"` + role + `","content":[{"type":"input_text","text":"` + text + `"}]}}` + "\n"
	}
	appendFile(t, path, `{"type":"session_meta","payload":{"id":"c1","cwd":"/w","timestamp":"2026-10-10T00:00:00Z"}}`+"\n"+msg("user", "fix the bug"))
	_, cur, err := ParseFrom(sf, Cursor{})
	if err != nil || cur.Title != "fix the bug" {
		t.Fatalf("full parse: cur=%+v err=%v", cur, err)
	}

	// The tail has no session_meta; its records still need the session's id, cwd and title.
	appendFile(t, path, msg("assistant", "done"))
	recs, _, err := ParseFrom(sf, cur)
	if err != nil || len(recs) != 1 {
		t.Fatalf("tail parse: recs=%v err=%v", texts(recs), err)
	}
	if r := recs[0]; r.SessionID != "c1" || r.Project != "/w" || r.Title != "fix the bug" || r.Timestamp == 0 {
		t.Fatalf("tail record lost session context: %+v", r)
	}
}
