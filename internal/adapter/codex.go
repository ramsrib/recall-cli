package adapter

import (
	"encoding/json"
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

func parseCodex(ls *lines, cur Cursor) ([]Record, Cursor, error) {
	var recs []Record
	next := cur
	err := ls.each(func(line []byte) {
		var l codexLine
		if json.Unmarshal(line, &l) != nil {
			return
		}
		switch l.Type {
		case "session_meta":
			var m codexMeta
			if json.Unmarshal(l.Payload, &m) == nil {
				next.SessionID, next.Project, next.Ts = m.ID, m.Cwd, parseISO(m.Timestamp)
			}
		case "response_item":
			var m codexMessage
			if json.Unmarshal(l.Payload, &m) != nil || m.Type != "message" {
				return
			}
			if m.Role != "user" && m.Role != "assistant" { // skip developer/system
				return
			}
			var parts []string
			for _, c := range m.Content {
				if c.Text != "" {
					parts = append(parts, c.Text)
				}
			}
			text := strings.Join(parts, "\n")
			if strings.TrimSpace(text) == "" {
				return
			}
			recs = append(recs, Record{
				Source:    "codex",
				SessionID: next.SessionID,
				Project:   next.Project,
				Timestamp: next.Ts,
				Role:      m.Role,
				Text:      text,
			})
		}
	})

	// Codex has no ai-title; derive one from the first "real" user message
	// (skip injected AGENTS.md / instruction blocks). Once found it sticks, so
	// a tail parse keeps the title an earlier pass derived.
	for _, r := range recs {
		if next.Title != "" {
			break
		}
		if r.Role != "user" {
			continue
		}
		t := strings.TrimSpace(r.Text)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "<") {
			continue
		}
		next.Title = firstLine(t, 80)
	}
	for i := range recs {
		recs[i].Title = next.Title
	}
	return recs, next, err
}
