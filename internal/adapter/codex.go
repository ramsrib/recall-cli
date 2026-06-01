package adapter

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

type codexLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type codexMeta struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
}

type codexMessage struct {
	Type    string `json:"type"` // want "message"
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func parseCodex(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var recs []Record
	var sessionID, project string
	var sessionTs int64

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 32<<20)
	for sc.Scan() {
		var l codexLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case "session_meta":
			var m codexMeta
			if json.Unmarshal(l.Payload, &m) == nil {
				sessionID, project, sessionTs = m.ID, m.Cwd, parseISO(m.Timestamp)
			}
		case "response_item":
			var m codexMessage
			if json.Unmarshal(l.Payload, &m) != nil || m.Type != "message" {
				continue
			}
			if m.Role != "user" && m.Role != "assistant" { // skip developer/system
				continue
			}
			var parts []string
			for _, c := range m.Content {
				if c.Text != "" {
					parts = append(parts, c.Text)
				}
			}
			text := strings.Join(parts, "\n")
			if strings.TrimSpace(text) == "" {
				continue
			}
			recs = append(recs, Record{
				Source:    "codex",
				SessionID: sessionID,
				Project:   project,
				Timestamp: sessionTs,
				Role:      m.Role,
				Text:      text,
			})
		}
	}

	// Codex has no ai-title; derive one from the first "real" user message
	// (skip injected AGENTS.md / instruction blocks).
	title := ""
	for _, r := range recs {
		if r.Role != "user" {
			continue
		}
		t := strings.TrimSpace(r.Text)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "<") {
			continue
		}
		title = firstLine(t, 80)
		break
	}
	for i := range recs {
		recs[i].Title = title
	}
	return recs, sc.Err()
}
