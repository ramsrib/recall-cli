package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAppendFileRefusesRowChangedSinceRead(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	chunk := func(text string) []Chunk { return []Chunk{{Source: "claude", Text: text, Embedding: []float32{1}}} }
	u := FileUpdate{Path: "/s.jsonl", Source: "claude", EmbedModel: "m", MtimeNs: 1, Size: 10, IndexedAt: 100, Cursor: "c1", Chunks: chunk("a")}
	if err := st.ReplaceFile(u); err != nil {
		t.Fatal(err)
	}
	read, _ := st.Manifest()

	// Another indexer (an older build that leaves cursor alone) re-indexes the
	// grown file meanwhile, in the same second, so every column matches; only
	// the chunks differ.
	old := u
	old.Chunks = append(chunk("a"), chunk("b")...)
	if err := st.ReplaceFile(old); err != nil {
		t.Fatal(err)
	}

	tail := u
	tail.IndexedAt, tail.Cursor, tail.Chunks = 300, "c2", chunk("b")
	if err := st.AppendFile(tail, read["/s.jsonl"], ""); !errors.Is(err, ErrStale) {
		t.Fatalf("err=%v, want ErrStale", err)
	}
	if _, n, _ := st.Counts(); n != 2 {
		t.Fatalf("stale append wrote chunks: %d chunks, want 2", n)
	}

	// Appending against the current row goes through.
	read, _ = st.Manifest()
	if err := st.AppendFile(tail, read["/s.jsonl"], ""); err != nil {
		t.Fatal(err)
	}
	if _, n, _ := st.Counts(); n != 3 {
		t.Fatalf("%d chunks after append, want 3", n)
	}
}
