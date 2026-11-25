package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ramsrib/recall-cli/internal/adapter"
	"github.com/ramsrib/recall-cli/internal/embed"
	"github.com/ramsrib/recall-cli/internal/index"
	"github.com/ramsrib/recall-cli/internal/search"
	"github.com/ramsrib/recall-cli/internal/store"
)

func TestAgySessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cli := agyFixture(t, home, "antigravity-cli")
	desktop := agyFixture(t, home, "antigravity")
	addAgySummary(t, cli, "cli-id", "Agy fixture", `["ssh://remote/work","file:///tmp/agent%20tools"]`)
	addAgySummary(t, desktop, "cli-id", "", `[]`)
	addAgySummary(t, desktop, "desktop-id", "Desktop fixture", `[]`)
	agyExec(t, cli, `INSERT INTO conversations_fts VALUES
		('cli-id', -1, 1795481348000000000, 1795481348000000000, '', ''),
		('cli-id', 1, 0, 0, '', 'The second answer chunk.'),
		('cli-id', 0, 0, 0, 'Neon model wiring.', 'First answer from Antigravity.')`)
	agyExec(t, desktop, `INSERT INTO conversations_fts VALUES
		('desktop-id', 0, 0, 0, '', 'Desktop response.')`)
	history := filepath.Join(home, ".gemini", "antigravity", "history.jsonl")
	prompts := `{"display":"unassociated prompt","timestamp":1795481347000,"workspace":"/wrong"}
{"display":"Desktop prompt fallback.","timestamp":1795481347000,"workspace":"/tmp/desktop","conversationId":"desktop-id"}
{"display":"/exit","timestamp":1795481349000,"workspace":"/tmp/desktop","conversationId":"desktop-id","type":"slash_command"}
{"display":"incomplete
`
	if err := os.WriteFile(history, []byte(prompts), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(home, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// Fixture text is embedded by a local test server through the normal index
	// path. The caller's configured Ollama is never used.
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input []string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		vs := make([][]float32, len(req.Input))
		for i := range vs {
			vs[i] = []float32{1, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vs})
	}))
	t.Cleanup(ollama.Close)
	t.Setenv("OLLAMA_HOST", ollama.URL)
	em := embed.New(embed.DefaultModel)
	ctx := context.Background()
	filter := store.Filter{Source: adapter.Agy, ExcludeExec: true}

	// List reads the registry before any chunks have been indexed. Keep a
	// writer open so the fixture rows are still in the WAL. An uncommitted
	// write must stay invisible to recall.
	tx, err := cli.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE conversation_summaries SET title='Uncommitted'`); err != nil {
		t.Fatal(err)
	}
	if err := index.RefreshSessions(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListSessions(filter)
	if err != nil || len(rows) != 2 {
		t.Fatalf("expected two unique sessions across both stores, got %+v (%v)", rows, err)
	}
	src, cwd, path, ok, err := st.SessionByID("cli-id")
	if err != nil || !ok || src != "agy" || cwd != "/tmp/agent tools" {
		t.Fatalf("unexpected CLI session: %q %q %q %v %v", src, cwd, path, ok, err)
	}
	if cmd := resumeCommand(src, "cli-id", cwd); cmd != "cd '/tmp/agent tools' && agy --conversation cli-id" {
		t.Fatalf("unexpected resume command: %q", cmd)
	}
	if rows[0].SessionID != "cli-id" || rows[0].Title != "Agy fixture" ||
		rows[0].LastTs != 1795481348 || rows[1].Project != "/tmp/desktop" ||
		rows[1].FirstTs != 1795481347 {
		t.Fatalf("unexpected Antigravity metadata: %+v", rows)
	}

	input, cached, stamp, err := summaryInput(st, src, "cli-id", path, false)
	want := "USER:\nNeon model wiring.\n\nASSISTANT:\nFirst answer from Antigravity.\n\nASSISTANT:\nThe second answer chunk.\n\n"
	if err != nil || cached || input != want {
		t.Fatalf("unexpected summary input: %q (%v, %v)", input, cached, err)
	}
	if err := st.SetSummary(src, "cli-id", stamp, "", "Fixture summary", 0); err != nil {
		t.Fatal(err)
	}
	if input, cached, _, err = summaryInput(st, src, "cli-id", path, false); err != nil || !cached || input != "Fixture summary" {
		t.Fatalf("unexpected cached summary: %q (%v, %v)", input, cached, err)
	}

	run := func() index.Stats {
		t.Helper()
		stats, err := index.Run(ctx, st, em, time.Now().Unix(), silentIndexProgress{})
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}
	assertMatch := func(phrase, id string) {
		t.Helper()
		hits, err := search.Run(st, em, phrase, search.ModeLexical, filter, 10, nil)
		if err != nil || len(hits) != 1 || hits[0].SessionID != id || hits[0].Source != adapter.Agy {
			t.Fatalf("%q: unexpected hits %+v (%v)", phrase, hits, err)
		}
	}
	if stats := run(); stats.New != 2 || stats.Chunks != 5 {
		t.Fatalf("unexpected first index: %+v", stats)
	}
	assertMatch("Neon model wiring", "cli-id")
	assertMatch("Desktop prompt fallback", "desktop-id")
	if stats := run(); stats.Skipped != 2 || stats.Chunks != 0 {
		t.Fatalf("expected unchanged sessions to be skipped: %+v", stats)
	}

	// Updating only the summary invalidates that conversation's index record.
	agyExec(t, cli, `UPDATE conversation_summaries SET last_modified_time='2026-11-24 00:50:00.123456+00:00' WHERE conversation_id='cli-id'`)
	if stats := run(); stats.Changed != 1 || stats.Skipped != 1 || stats.Appended != 0 {
		t.Fatalf("expected only the updated conversation to be replaced: %+v", stats)
	}
	// A later FTS-only update must also be picked up, without duplicating text.
	agyExec(t, cli, `UPDATE conversations_fts SET last_modified_time=1795481400000000000 WHERE chunk_index=-1`)
	agyExec(t, cli, `UPDATE conversations_fts SET agent_body='Revised response from Gemini.' WHERE chunk_index=0`)
	if stats := run(); stats.Changed != 1 || stats.Skipped != 1 || stats.Chunks != 3 {
		t.Fatalf("expected a per-conversation FTS refresh: %+v", stats)
	}
	assertMatch("Revised", "cli-id")
	hits, err := search.Run(st, em, "first", search.ModeLexical, filter, 10, nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("obsolete chunks remain: %+v (%v)", hits, err)
	}
	input, cached, _, err = summaryInput(st, src, "cli-id", path, false)
	if err != nil || cached || !strings.Contains(input, "Revised response from Gemini.") {
		t.Fatalf("expected updated summary input: %q (%v, %v)", input, cached, err)
	}
}

func agyFixture(t *testing.T, home, name string) *sql.DB {
	t.Helper()
	dir := filepath.Join(home, ".gemini", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	agyExec(t, db, "PRAGMA journal_mode=WAL")
	agyExec(t, db, "PRAGMA wal_autocheckpoint=0")
	agyExec(t, db, `CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY, title TEXT NOT NULL, workspace_uris TEXT NOT NULL,
		step_count INTEGER NOT NULL, last_modified_time DATETIME NOT NULL, last_user_input_time DATETIME NOT NULL)`)
	agyExec(t, db, `CREATE VIRTUAL TABLE conversations_fts USING fts5(
		conversation_id UNINDEXED, chunk_index UNINDEXED, last_modified_time UNINDEXED,
		last_step_created_at UNINDEXED, user_body, agent_body)`)
	return db
}

func agyExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func addAgySummary(t *testing.T, db *sql.DB, id, title, uris string) {
	t.Helper()
	agyExec(t, db, `INSERT INTO conversation_summaries VALUES (?, ?, ?, 4,
		'2026-11-24 00:49:07.123456+00:00', '2026-11-24 00:49:07.001+00:00')`, id, title, uris)
}

type silentIndexProgress struct{}

func (silentIndexProgress) Planned(int, int)             {}
func (silentIndexProgress) Started(int, string, string)  {}
func (silentIndexProgress) Embedded(int, int)            {}
func (silentIndexProgress) Finished(int, index.FileStat) {}
func (silentIndexProgress) Note(string)                  {}
