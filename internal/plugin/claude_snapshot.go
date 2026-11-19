package plugin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"cpa-codex-guard/internal/guard"
	"cpa-codex-guard/internal/types"
)

const (
	claudeSnapshotTTL         = 2 * time.Hour
	claudeSnapshotMaxBytes    = 8 << 20
	claudeSnapshotMaxSessions = 256
)

type claudeThinkingSnapshot struct {
	Blocks []json.RawMessage
	SeenAt time.Time
	Model  string
}

// claudeSessionID intentionally uses the Claude-specific header instead of
// the generic Codex session keys. It keeps Claude replay state separate from
// the existing fuzzy circuit-breaker state.
func claudeSessionID(headers http.Header) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id"))
}

func claudeStreamRequest(headers http.Header, stream bool) bool {
	if stream {
		return true
	}
	if headers == nil {
		return false
	}
	return strings.Contains(strings.ToLower(headers.Get("Accept")), "text/event-stream")
}

func claudeRequestModel(body []byte, fallback string) (string, bool) {
	var root struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &root) != nil {
		return fallback, strings.HasPrefix(fallback, "claude-")
	}
	if root.Model == "" {
		root.Model = fallback
	}
	return root.Model, strings.HasPrefix(root.Model, "claude-")
}

func latestAssistantThinkingBlocks(body []byte) ([]json.RawMessage, bool) {
	var root struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil {
		return nil, false
	}
	latest := -1
	for i := range root.Messages {
		if root.Messages[i].Role == "assistant" {
			latest = i
		}
	}
	if latest < 0 {
		return nil, false
	}
	var blocks []json.RawMessage
	if json.Unmarshal(root.Messages[latest].Content, &blocks) != nil {
		return nil, false
	}
	result := make([]json.RawMessage, 0, len(blocks))
	for _, raw := range blocks {
		var block struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &block) == nil && (block.Type == "thinking" || block.Type == "redacted_thinking") {
			result = append(result, append(json.RawMessage(nil), raw...))
		}
	}
	return result, len(result) > 0
}

func (e *Engine) checkClaudeSnapshot(session, model string, body []byte) *types.RequestInterceptResponse {
	if session == "" || !strings.HasPrefix(model, "claude-") {
		return nil
	}
	blocks, ok := latestAssistantThinkingBlocks(body)
	if !ok {
		return nil
	}

	e.claudeSnapshotMu.Lock()
	snapshot, exists := e.claudeSnapshots[session]
	if exists && time.Since(snapshot.SeenAt) > claudeSnapshotTTL {
		delete(e.claudeSnapshots, session)
		exists = false
	}
	e.claudeSnapshotMu.Unlock()
	if !exists || len(snapshot.Blocks) != len(blocks) {
		return nil
	}
	for i := range blocks {
		if !jsonEqual(snapshot.Blocks[i], blocks[i]) {
			return guard.ShortCircuitClaudeThinkingMismatch()
		}
	}
	return nil
}

func (e *Engine) saveClaudeSnapshot(session, model string, blocks []json.RawMessage) {
	if session == "" || len(blocks) == 0 {
		return
	}
	e.claudeSnapshotMu.Lock()
	defer e.claudeSnapshotMu.Unlock()
	if len(e.claudeSnapshots) >= claudeSnapshotMaxSessions {
		var oldest string
		var oldestAt time.Time
		for key, snapshot := range e.claudeSnapshots {
			if oldest == "" || snapshot.SeenAt.Before(oldestAt) {
				oldest, oldestAt = key, snapshot.SeenAt
			}
		}
		if oldest != "" {
			delete(e.claudeSnapshots, oldest)
		}
	}
	e.claudeSnapshots[session] = claudeThinkingSnapshot{
		Blocks: cloneRawMessages(blocks),
		SeenAt: time.Now(),
		Model:  model,
	}
}

func cloneRawMessages(blocks []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, len(blocks))
	for i := range blocks {
		result[i] = append(json.RawMessage(nil), blocks[i]...)
	}
	return result
}

func jsonEqual(a, b json.RawMessage) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflectJSONEqual(left, right)
}

func reflectJSONEqual(left, right any) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

// parseClaudeThinkingSSE returns only a complete assistant response. A
// cancelled/truncated stream is deliberately ignored so it cannot poison the
// next request's replay validation.
func parseClaudeThinkingSSE(raw []byte) ([]json.RawMessage, bool) {
	if len(raw) == 0 || len(raw) > claudeSnapshotMaxBytes {
		return nil, false
	}
	blocks := map[int]map[string]any{}
	complete := false
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), claudeSnapshotMaxBytes)
	var eventData []string
	flush := func() {
		if len(eventData) == 0 {
			return
		}
		var event struct {
			Type         string         `json:"type"`
			Index        int            `json:"index"`
			ContentBlock map[string]any `json:"content_block"`
			Delta        map[string]any `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.Join(eventData, "\n")), &event) != nil {
			eventData = nil
			return
		}
		switch event.Type {
		case "content_block_start":
			if typ, ok := event.ContentBlock["type"].(string); ok && (typ == "thinking" || typ == "redacted_thinking") {
				blocks[event.Index] = map[string]any{"type": typ}
				for _, key := range []string{"thinking", "data", "signature"} {
					if value, ok := event.ContentBlock[key].(string); ok {
						blocks[event.Index][key] = value
					}
				}
			}
		case "content_block_delta":
			block, ok := blocks[event.Index]
			if !ok {
				break
			}
			deltaType, _ := event.Delta["type"].(string)
			key := ""
			switch deltaType {
			case "thinking_delta":
				key = "thinking"
			case "signature_delta":
				key = "signature"
			case "redacted_thinking_delta":
				key = "data"
			}
			if key != "" {
				if text, ok := event.Delta[key].(string); ok {
					old, _ := block[key].(string)
					block[key] = old + text
				}
			}
		case "message_stop":
			complete = true
		}
		eventData = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			eventData = append(eventData, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	if scanner.Err() != nil || !complete || len(blocks) == 0 {
		return nil, false
	}
	result := make([]json.RawMessage, 0, len(blocks))
	for index := 0; index < len(blocks); index++ {
		block, ok := blocks[index]
		if !ok {
			continue
		}
		rawBlock, err := json.Marshal(block)
		if err != nil {
			return nil, false
		}
		result = append(result, rawBlock)
	}
	return result, len(result) > 0
}
