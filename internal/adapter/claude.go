package adapter

import (
	"bufio"
	"encoding/json"
	"os"
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

func parseClaude(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var recs []Record
	title := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 32<<20) // some lines are large
	for sc.Scan() {
		var l claudeLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.Type == "ai-title" && l.AiTitle != "" {
			title = l.AiTitle
			continue
		}
		if (l.Type != "user" && l.Type != "assistant") || l.Message == nil {
			continue
		}
		text := extractClaudeContent(l.Message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		recs = append(recs, Record{
			Source:    "claude",
			SessionID: l.SessionID,
			Project:   l.Cwd,
			Timestamp: parseISO(l.Timestamp),
			Role:      l.Message.Role,
			Text:      text,
		})
	}
	for i := range recs {
		recs[i].Title = title
	}
	return recs, sc.Err()
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
