package sessionroute

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

// SessionID accepts only explicit stable signals; it does not infer identity
// from prompt text, which could merge unrelated callers or repeated probes.
func SessionID(headers http.Header, body []byte, metadata map[string]any) string {
	// CPA has already reconciled explicit identities across transports when this
	// field is present. It must take precedence over unnormalized client headers.
	if id, ok := metadata["canonical_session_id"].(string); ok && validSession(id) {
		if normalized := canonicalSessionID(id); normalized != "" {
			return normalized
		}
	}
	for _, field := range []string{"Session-Id", "Session_id", "X-Session-Id", "X-Session-Affinity"} {
		if id := headers.Get(field); validSession(id) {
			if normalized := canonicalSessionID(id); normalized != "" {
				return normalized
			}
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
		return canonicalSessionID(root.SessionID)
	}
	if validSession(root.PromptCacheKey) {
		return canonicalSessionID(root.PromptCacheKey)
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
		return canonicalSessionID(conversation)
	}
	return ""
}

func validSession(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256
}

var uuidSession = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Match CPA sdk/cliproxy/session.NormalizeToCanonicalUUID without depending on
// the host module: strip legacy wrappers, lowercase UUIDs, or project opaque IDs
// to UUIDv8 using the host's domain separator. Keep parity vectors in tests.
func canonicalSessionID(value string) string {
	value = strings.TrimSpace(value)
	prefixes := []string{"lcp:v1:", "lcp:", "ctx:v1:", "ctx:", "codex:", "claude:", "header:", "session:", "affinity:", "slot:", "task:", "conv:", "thread:", "clientreq:", "geminicache:", "pck:", "user:", "execution:", "agy:", "derived:"}
	for {
		stripped := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(value, prefix) {
				value = strings.TrimSpace(strings.TrimPrefix(value, prefix))
				stripped = true
				break
			}
		}
		if !stripped {
			break
		}
	}
	if value == "" {
		return ""
	}
	if uuidSession.MatchString(value) {
		return strings.ToLower(value)
	}
	if index := strings.IndexByte(value, ':'); index > 0 && uuidSession.MatchString(strings.TrimSpace(value[index+1:])) {
		return strings.ToLower(strings.TrimSpace(value[index+1:]))
	}
	sum := sha256.Sum256([]byte("cpa:canonical-uuid:v1\x00" + value))
	u := sum[:16]
	u[6] = (u[6] & 0x0f) | 0x80
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", u[:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
