package main

import "testing"

func TestFitWidth(t *testing.T) {
	long := "  [ 4/10]  embedding 384/5,477 chunks (7%)  11.9s  ~/.claude/projects/x/68161a98.jsonl"
	for _, w := range []int{0, 1, 2, 7, 8, 20, 80, 200} {
		got := []rune(fitWidth(long, w))
		if len(got) >= max(w, 1) && len(got) != len([]rune(long)) {
			t.Errorf("width %d: got %d runes %q", w, len(got), string(got))
		}
	}
	if s := fitWidth("short", 80); s != "short" {
		t.Errorf("short line changed: %q", s)
	}
}
