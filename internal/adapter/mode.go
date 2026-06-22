package adapter

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// Mode classifies a session as "interactive" (a human-driven TUI/desktop
// session) or "exec" (a non-interactive, programmatic run). The signal lives in
// each tool's transcript header:
//   - Codex: session_meta.source=="exec" (originator "codex_exec") — e.g. the
//     `codex exec` calls recall itself uses to generate titles/summaries.
//   - Claude: the per-message `entrypoint` ("sdk", "sdk-cli", … => headless
//     `claude -p`/SDK) or promptSource=="sdk"; interactive sessions are
//     "cli"/"claude-desktop".
//
// Anything unrecognized defaults to "interactive" so a real session is never
// hidden by mistake. Reads only the file's header, so it's cheap to call per
// session at list/search time (no schema change or reindex required).
func Mode(path, source string) string {
	switch source {
	case "codex":
		return codexMode(path)
	case "claude":
		return claudeMode(path)
	}
	return "interactive"
}

// codexMode reads only the first record (always session_meta) and checks source.
func codexMode(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "interactive"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	if !sc.Scan() {
		return "interactive"
	}
	var l struct {
		Type    string `json:"type"`
		Payload struct {
			Source     string `json:"source"`
			Originator string `json:"originator"`
		} `json:"payload"`
	}
	if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "session_meta" {
		return "interactive"
	}
	if l.Payload.Source == "exec" || l.Payload.Originator == "codex_exec" {
		return "exec"
	}
	return "interactive"
}

// claudeMode scans the first handful of records for headless markers: an
// `entrypoint` beginning with "sdk" (covers "sdk", "sdk-cli", …) or
// promptSource=="sdk" (a `claude -p`/SDK run). The first record carrying a real
// interactive entrypoint ("cli"/"claude-desktop") settles it as interactive.
func claudeMode(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "interactive"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for i := 0; i < 40 && sc.Scan(); i++ {
		var m struct {
			Entrypoint   string `json:"entrypoint"`
			PromptSource string `json:"promptSource"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if strings.HasPrefix(m.Entrypoint, "sdk") || m.PromptSource == "sdk" {
			return "exec"
		}
		if m.Entrypoint != "" {
			return "interactive"
		}
	}
	return "interactive"
}
