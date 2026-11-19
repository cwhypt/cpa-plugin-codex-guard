package probecache

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cpa-codex-guard/internal/types"
)

// Only stateless, single-text-input Responses requests are replayable.
func safeResponsesRequest(root map[string]json.RawMessage) bool {
	var input string
	if json.Unmarshal(root["input"], &input) != nil {
		var items []map[string]json.RawMessage
		if json.Unmarshal(root["input"], &items) != nil || len(items) != 1 {
			return false
		}
		for key := range items[0] {
			if key != "role" && key != "type" && key != "content" {
				return false
			}
		}
		var role, kind string
		_ = json.Unmarshal(items[0]["role"], &role)
		_ = json.Unmarshal(items[0]["type"], &kind)
		if role != "user" || (kind != "" && kind != "message") {
			return false
		}
		if json.Unmarshal(items[0]["content"], &input) != nil {
			var parts []map[string]json.RawMessage
			if json.Unmarshal(items[0]["content"], &parts) != nil || len(parts) != 1 || len(parts[0]) != 2 {
				return false
			}
			_ = json.Unmarshal(parts[0]["type"], &kind)
			if kind != "input_text" || json.Unmarshal(parts[0]["text"], &input) != nil {
				return false
			}
		}
	}
	if input == "" {
		return false
	}
	for _, field := range []string{"previous_response_id", "conversation"} {
		if value, ok := root[field]; ok && string(value) != "null" {
			return false
		}
	}
	for _, field := range []string{"tools", "include"} {
		if value, ok := root[field]; ok && string(value) != "null" && string(value) != "[]" {
			return false
		}
	}
	for _, field := range []string{"background", "store"} {
		if value, ok := root[field]; ok && string(value) != "null" && string(value) != "false" {
			return false
		}
	}
	return true
}

func newReplayID(prefix string) (string, bool) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", false
	}
	return prefix + hex.EncodeToString(raw[:]), true
}

// formatResponsesReplay rejects samples whose complete references cannot be
// reconstructed; the caller will pass them through to the upstream provider.
func formatResponsesReplay(sample *Sample, stream bool, model string, usage map[string]any) *types.RequestInterceptResponse {
	var original map[string]any
	if json.Unmarshal(sample.RawResponse, &original) != nil || original["object"] != "response" || original["status"] != "completed" {
		return nil
	}
	oldResponseID, ok := original["id"].(string)
	items, itemsOK := original["output"].([]any)
	if !ok || oldResponseID == "" || !itemsOK || len(items) != 1 {
		return nil
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["type"] != "message" || item["role"] != "assistant" || item["status"] != "completed" {
		return nil
	}
	oldItemID, ok := item["id"].(string)
	parts, partsOK := item["content"].([]any)
	if !ok || oldItemID == "" || !partsOK || len(parts) != 1 {
		return nil
	}
	part, ok := parts[0].(map[string]any)
	if !ok || part["type"] != "output_text" || part["text"] != sample.Text {
		return nil
	}
	if annotations, ok := part["annotations"].([]any); ok && len(annotations) != 0 {
		return nil
	}
	responseID, ok := newReplayID("resp_")
	if !ok {
		return nil
	}
	itemID, ok := newReplayID("msg_")
	if !ok {
		return nil
	}
	oldIDs := []string{oldResponseID, oldItemID}
	if attribution, exists := usage["attribution"]; exists && attribution != nil {
		a, ok := attribution.(map[string]any)
		if !ok {
			return nil
		}
		oldItems, ok := a["items"].(map[string]any)
		if !ok {
			return nil
		}
		if _, ok := oldItems[oldItemID]; !ok {
			return nil
		}
		newItems := make(map[string]any, len(oldItems))
		for id, value := range oldItems {
			if _, ok := value.(map[string]any); id == "" || !ok {
				return nil
			}
			newID := itemID
			if id != oldItemID {
				newID, ok = newReplayID("msg_")
				if !ok {
					return nil
				}
				oldIDs = append(oldIDs, id)
			}
			newItems[newID] = value
		}
		a["items"] = newItems
	}
	item["id"] = itemID
	original["id"] = responseID
	original["created_at"] = time.Now().Unix()
	if _, exists := original["completed_at"]; exists {
		original["completed_at"] = original["created_at"]
	}
	original["model"] = model
	original["usage"] = usage
	completed, err := json.Marshal(original)
	if err != nil {
		return nil
	}
	for _, id := range oldIDs {
		if strings.Contains(string(completed), id) {
			return nil
		}
	}
	headers := make(http.Header)
	if !stream {
		headers.Set("Content-Type", "application/json")
		return &types.RequestInterceptResponse{Terminate: true, StatusCode: 200, ResponseBody: completed, ResponseHeaders: headers}
	}
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	created := make(map[string]any, len(original))
	for key, value := range original {
		created[key] = value
	}
	created["status"], created["output"], created["usage"] = "in_progress", []any{}, nil
	if _, exists := created["completed_at"]; exists {
		created["completed_at"] = nil
	}
	added := map[string]any{"id": itemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
	emptyPart := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	events := []map[string]any{
		{"type": "response.created", "response": created},
		{"type": "response.in_progress", "response": created},
		{"type": "response.output_item.added", "output_index": 0, "item": added},
		{"type": "response.content_part.added", "item_id": itemID, "output_index": 0, "content_index": 0, "part": emptyPart},
		{"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": sample.Text},
		{"type": "response.output_text.done", "item_id": itemID, "output_index": 0, "content_index": 0, "text": sample.Text},
		{"type": "response.content_part.done", "item_id": itemID, "output_index": 0, "content_index": 0, "part": part},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": original},
	}
	var body strings.Builder
	for i, event := range events {
		event["sequence_number"] = i
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil
		}
		fmt.Fprintf(&body, "event: %s\ndata: %s\n\n", event["type"], encoded)
	}
	body.WriteString("data: [DONE]\n\n")
	return &types.RequestInterceptResponse{Terminate: true, StatusCode: 200, ResponseBody: []byte(body.String()), ResponseHeaders: headers}
}
