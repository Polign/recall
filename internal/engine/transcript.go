package engine

import (
	"fmt"
	"strings"
)

// Message is one turn of a conversation to remember.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// MaxTranscript bounds a conversation remembered at once, as the text tools
// bound a passage.
const MaxTranscript = 32768

// Transcript writes a conversation as text an extractor reads by speaker,
// one "User:" or "Assistant:" line per turn, so only what the user said or
// agreed to becomes a statement about them. Roles are user and assistant.
func Transcript(messages []Message) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("recall: a conversation needs at least one message")
	}
	var b strings.Builder
	for i, m := range messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			return "", fmt.Errorf("recall: message %d has no content", i)
		}
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "user":
			b.WriteString("User: ")
		case "assistant":
			b.WriteString("Assistant: ")
		default:
			return "", fmt.Errorf("recall: message %d has role %q; roles are user and assistant", i, m.Role)
		}
		b.WriteString(strings.Join(strings.Fields(content), " "))
		b.WriteByte('\n')
	}
	if b.Len() > MaxTranscript {
		return "", fmt.Errorf("recall: the conversation is %d bytes; the most remembered at once is %d", b.Len(), MaxTranscript)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
