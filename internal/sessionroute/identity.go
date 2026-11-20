package sessionroute

import (
	"encoding/json"
	"net/http"
	"strings"
)

// SessionID accepts only explicit stable signals; it does not infer identity
// from prompt text, which could merge unrelated callers or repeated probes.
func SessionID(headers http.Header, body []byte, metadata map[string]any) string {
	for _, field := range []struct{ name, prefix string }{
		{"Session-Id", "session:"}, {"Session_id", "session:"},
		{"X-Session-Id", "header:"}, {"X-Session-Affinity", "affinity:"},
	} {
		if id := headers.Get(field.name); validSession(id) {
			return field.prefix + strings.TrimSpace(id)
		}
	}
	var root struct {
		SessionID      string          `json:"session_id"`
		PromptCacheKey string          `json:"prompt_cache_key"`
		Conversation   json.RawMessage `json:"conversation"`
	}
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	if validSession(root.SessionID) {
		return "body:" + strings.TrimSpace(root.SessionID)
	}
	if validSession(root.PromptCacheKey) {
		return "pck:" + strings.TrimSpace(root.PromptCacheKey)
	}
	var conversation string
	if json.Unmarshal(root.Conversation, &conversation) != nil {
		var object struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(root.Conversation, &object)
		conversation = object.ID
	}
	if validSession(conversation) {
		return "conversation:" + strings.TrimSpace(conversation)
	}
	if id, ok := metadata["canonical_session_id"].(string); ok && validSession(id) {
		return "canonical:" + strings.TrimSpace(id)
	}
	return ""
}

func validSession(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}
