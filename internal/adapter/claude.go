package adapter

import (
	"encoding/json"
	"strings"
)

type claudeLine struct {
	Type      string         `json:"type"`
	AiTitle   string         `json:"aiTitle"`
	SessionID string         `json:"sessionId"`
	Cwd       string         `json:"cwd"`
	Timestamp string         `json:"timestamp"`
	Message   *claudeMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func parseClaude(path string, cur Cursor) ([]Record, Cursor, error) {
	var recs []Record
	next := cur
	end, err := scanLines(path, cur.Offset, func(line []byte) {
		var l claudeLine
		if json.Unmarshal(line, &l) != nil {
			return
		}
		if l.Type == "ai-title" && l.AiTitle != "" {
			next.Title = l.AiTitle
			return
		}
		if (l.Type != "user" && l.Type != "assistant") || l.Message == nil {
			return
		}
		text := extractClaudeContent(l.Message.Content)
		if strings.TrimSpace(text) == "" {
			return
		}
		recs = append(recs, Record{
			Source:    "claude",
			SessionID: l.SessionID,
			Project:   l.Cwd,
			Timestamp: parseISO(l.Timestamp),
			Role:      l.Message.Role,
			Text:      text,
		})
	})
	next.Offset = end
	for i := range recs {
		recs[i].Title = next.Title
	}
	return recs, next, err
}

// extractClaudeContent pulls human-readable text from a message's content,
// which is either a JSON string or a list of typed blocks.
func extractClaudeContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				parts = append(parts, b.Thinking)
			}
			// tool_use / tool_result intentionally skipped
		}
	}
	return strings.Join(parts, "\n")
}
