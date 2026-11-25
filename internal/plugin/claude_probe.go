package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"cpa-codex-guard/internal/types"
)

// claudeHealthProbe handles only the observed non-streaming one-token probe.
func claudeHealthProbe(req *types.RequestInterceptRequest, body []byte) *types.RequestInterceptResponse {
	if req.SourceFormat != "claude" || req.Stream || len(body) > 1024 {
		return nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || len(root) != 3 {
		return nil
	}
	var model string
	var maxTokens int
	var messages []map[string]json.RawMessage
	if json.Unmarshal(root["model"], &model) != nil || strings.TrimSpace(model) == "" ||
		json.Unmarshal(root["max_tokens"], &maxTokens) != nil || maxTokens != 1 ||
		json.Unmarshal(root["messages"], &messages) != nil || len(messages) != 1 || len(messages[0]) != 2 {
		return nil
	}
	var role, content string
	if json.Unmarshal(messages[0]["role"], &role) != nil || role != "user" ||
		json.Unmarshal(messages[0]["content"], &content) != nil || content != "." {
		return nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil
	}
	response, err := json.Marshal(map[string]any{
		"id": "msg_" + hex.EncodeToString(id[:]), "type": "message", "role": "assistant", "model": model,
		"content":     []any{map[string]string{"type": "text", "text": "It"}},
		"stop_reason": "max_tokens", "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	})
	if err != nil {
		return nil
	}
	return &types.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusOK, ResponseBody: response, ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}}}
}
