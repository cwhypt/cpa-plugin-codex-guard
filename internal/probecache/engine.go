package probecache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cpa-codex-guard/internal/types"
)

type Engine struct {
	store          *Store
	maxInputBytes  int
	maxOutputChars int
}

func NewEngine(store *Store, maxInputBytes, maxOutputChars int) *Engine {
	if maxInputBytes <= 0 {
		maxInputBytes = 20480 // 20KB
	}
	if maxOutputChars <= 0 {
		maxOutputChars = 200
	}
	return &Engine{
		store:          store,
		maxInputBytes:  maxInputBytes,
		maxOutputChars: maxOutputChars,
	}
}

func (e *Engine) Store() *Store {
	return e.store
}

// ComputeInputHash 检查输入是否符合 <= 20KB，并生成规范化输入指纹
func (e *Engine) ComputeInputHash(body []byte) (hash string, eligible bool, isStream bool, format string) {
	if len(body) == 0 || len(body) > e.maxInputBytes {
		return "", false, false, ""
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return "", false, false, ""
	}

	var stream bool
	if streamRaw, ok := root["stream"]; ok {
		_ = json.Unmarshal(streamRaw, &stream)
	}

	var model string
	if mRaw, ok := root["model"]; ok {
		_ = json.Unmarshal(mRaw, &model)
	}

	// 识别协议格式与提取提示词
	format = "chat_completions"
	normalizedPrompts := make([]string, 0, 4)

	if msgsRaw, ok := root["messages"]; ok {
		format = "chat_completions"
		var msgs []map[string]any
		if err := json.Unmarshal(msgsRaw, &msgs); err == nil {
			for _, m := range msgs {
				role, _ := m["role"].(string)
				contentStr := extractContentString(m["content"])
				normalizedPrompts = append(normalizedPrompts, fmt.Sprintf("%s:%s", role, contentStr))
			}
		}
	} else if inputRaw, ok := root["input"]; ok {
		format = "responses"
		var inputs []map[string]any
		if err := json.Unmarshal(inputRaw, &inputs); err == nil {
			for _, in := range inputs {
				role, _ := in["role"].(string)
				contentStr := extractContentString(in["content"])
				normalizedPrompts = append(normalizedPrompts, fmt.Sprintf("%s:%s", role, contentStr))
			}
		}
	} else {
		// 其它非对话协议不进入 probe_cache
		return "", false, stream, ""
	}

	if len(normalizedPrompts) == 0 {
		return "", false, stream, format
	}

	// 计算哈希：只匹配 model + input 内容（不含 format / session / UUID 等）
	h := sha256.New()
	h.Write([]byte(model))
	for _, p := range normalizedPrompts {
		h.Write([]byte("|"))
		h.Write([]byte(p))
	}

	return hex.EncodeToString(h.Sum(nil)), true, stream, format
}

// ExtractOutputText 从上游响应中提取纯文本输出并校验长度限制
func (e *Engine) ExtractOutputText(format string, respBody []byte) (text string, eligible bool) {
	if len(respBody) == 0 {
		return "", false
	}

	var root map[string]any
	if err := json.Unmarshal(respBody, &root); err != nil {
		return "", false
	}

	switch format {
	case "chat_completions":
		if choices, ok := root["choices"].([]any); ok && len(choices) > 0 {
			if first, ok := choices[0].(map[string]any); ok {
				if msg, ok := first["message"].(map[string]any); ok {
					text, _ = msg["content"].(string)
				}
			}
		}
	case "responses":
		if output, ok := root["output"].([]any); ok && len(output) > 0 {
			for _, item := range output {
				if itemMap, ok := item.(map[string]any); ok {
					if content, ok := itemMap["content"].([]any); ok && len(content) > 0 {
						for _, c := range content {
							if cMap, ok := c.(map[string]any); ok {
								if t, ok := cMap["text"].(string); ok {
									text += t
								}
							}
						}
					}
				}
			}
		}
	default:
		// 备用通用字段提取
		if choices, ok := root["choices"].([]any); ok && len(choices) > 0 {
			if first, ok := choices[0].(map[string]any); ok {
				if msg, ok := first["message"].(map[string]any); ok {
					text, _ = msg["content"].(string)
				}
			}
		}
	}

	charCount := len([]rune(text))
	if charCount == 0 || charCount > e.maxOutputChars {
		return text, false
	}

	return text, true
}

// ExtractStreamText 从流式 SSE chunk 历史中拼接纯文本输出并校验长度限制。
// 支持 chat.completion.chunk delta 与 responses event SSE 两种格式。
// 同时容忍 frame 间缺少空行分隔的紧凑 SSE（event:/data: 行直接相连）。
func (e *Engine) ExtractStreamText(format string, chunks [][]byte) (text string, eligible bool) {
	if len(chunks) == 0 {
		return "", false
	}

	for _, chunk := range chunks {
		s := string(chunk)
		// 规范化：确保 event:/data: 行各自独立，即使原始帧缺失分隔符
		s = strings.ReplaceAll(s, "event:", "\nevent:")
		s = strings.ReplaceAll(s, "data:", "\ndata:")
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}

			var ev map[string]any
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}

			switch format {
			case "chat_completions":
				if choices, ok := ev["choices"].([]any); ok && len(choices) > 0 {
					if first, ok := choices[0].(map[string]any); ok {
						if delta, ok := first["delta"].(map[string]any); ok {
							if t, ok := delta["content"].(string); ok {
								text += t
							}
						}
					}
				}
			case "responses":
				if t, ok := ev["delta"].(string); ok {
					text += t
				} else if resp, ok := ev["response"].(map[string]any); ok {
					// response.completed 携带全文：重置累计，避免与 delta 重复
					full := extractResponsesOutputText(resp)
					if full != "" {
						text = full
					}
				}
			default:
				if t, ok := ev["delta"].(string); ok {
					text += t
				}
			}
		}
	}

	charCount := len([]rune(text))
	if charCount == 0 || charCount > e.maxOutputChars {
		return text, false
	}

	return text, true
}

func extractResponsesOutputText(resp map[string]any) string {
	res := ""
	if output, ok := resp["output"].([]any); ok {
		for _, item := range output {
			if itemMap, ok := item.(map[string]any); ok {
				if content, ok := itemMap["content"].([]any); ok {
					for _, c := range content {
						if cMap, ok := c.(map[string]any); ok {
							if t, ok := cMap["text"].(string); ok {
								res += t
							}
						}
					}
				}
			}
		}
	}
	return res
}

// FormatResponse 构造缓存命中的返回响应。哈希只匹配 model + input 内容，
// 因此回放时按请求的 format / isStream 合成对应协议的响应体。
func (e *Engine) FormatResponse(sample *Sample, isStream bool, format, model string) *types.RequestInterceptResponse {
	if sample == nil {
		return nil
	}

	nowUnix := time.Now().Unix()
	respHeaders := make(http.Header)
	cacheID := fmt.Sprintf("chatcmpl-cached-%d", time.Now().UnixNano())

	if format == "responses" {
		resp := synthesizeResponsesObject(cacheID, model, sample.Text, nowUnix)
		if !isStream {
			respHeaders.Set("Content-Type", "application/json")
			bodyBytes, _ := json.Marshal(resp)
			return &types.RequestInterceptResponse{
				Terminate:       true,
				StatusCode:      200,
				ResponseBody:    bodyBytes,
				ResponseHeaders: respHeaders,
			}
		}

		// responses 流式：标准 Responses SSE 事件序列
		respHeaders.Set("Content-Type", "text/event-stream")
		respHeaders.Set("Cache-Control", "no-cache")
		respHeaders.Set("Connection", "keep-alive")

		createdResp := synthesizeResponsesObject(cacheID, model, sample.Text, nowUnix)
		createdResp["status"] = "in_progress"
		createdResp["output"] = []any{}
		b1, _ := json.Marshal(map[string]any{"type": "response.created", "response": createdResp})

		b2, _ := json.Marshal(map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       "msg_cached_0",
			"output_index":  0,
			"content_index": 0,
			"delta":         sample.Text,
		})

		completedResp := synthesizeResponsesObject(cacheID, model, sample.Text, nowUnix)
		b3, _ := json.Marshal(map[string]any{"type": "response.completed", "response": completedResp})

		ssePayload := fmt.Sprintf("event: response.created\ndata: %s\n\nevent: response.output_text.delta\ndata: %s\n\nevent: response.completed\ndata: %s\n\ndata: [DONE]\n\n", string(b1), string(b2), string(b3))
		return &types.RequestInterceptResponse{
			Terminate:       true,
			StatusCode:      200,
			ResponseBody:    []byte(ssePayload),
			ResponseHeaders: respHeaders,
		}
	}

	if !isStream {
		// chat_completions 非流式
		respHeaders.Set("Content-Type", "application/json")

		body := map[string]any{
			"id":      cacheID,
			"object":  "chat.completion",
			"created": nowUnix,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": sample.Text},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     0,
				"completion_tokens": len([]rune(sample.Text)),
				"total_tokens":      len([]rune(sample.Text)),
			},
		}
		bodyBytes, _ := json.Marshal(body)
		return &types.RequestInterceptResponse{
			Terminate:       true,
			StatusCode:      200,
			ResponseBody:    bodyBytes,
			ResponseHeaders: respHeaders,
		}
	}

	// chat_completions 流式：标准 chat.completion.chunk SSE
	respHeaders.Set("Content-Type", "text/event-stream")
	respHeaders.Set("Cache-Control", "no-cache")
	respHeaders.Set("Connection", "keep-alive")

	// 构造分块
	chunk1 := map[string]any{
		"id":      cacheID,
		"object":  "chat.completion.chunk",
		"created": nowUnix,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
				},
				"finish_reason": nil,
			},
		},
	}
	b1, _ := json.Marshal(chunk1)

	chunk2 := map[string]any{
		"id":      cacheID,
		"object":  "chat.completion.chunk",
		"created": nowUnix,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"content": sample.Text,
				},
				"finish_reason": nil,
			},
		},
	}
	b2, _ := json.Marshal(chunk2)

	chunk3 := map[string]any{
		"id":      cacheID,
		"object":  "chat.completion.chunk",
		"created": nowUnix,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	}
	b3, _ := json.Marshal(chunk3)

	ssePayload := fmt.Sprintf("data: %s\n\ndata: %s\n\ndata: %s\n\ndata: [DONE]\n\n", string(b1), string(b2), string(b3))

	return &types.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      200,
		ResponseBody:    []byte(ssePayload),
		ResponseHeaders: respHeaders,
	}
}

// synthesizeResponsesObject 构造一个最小合法的 OpenAI Responses API response 对象。
func synthesizeResponsesObject(id, model, text string, nowUnix int64) map[string]any {
	outputText := map[string]any{
		"type":        "output_text",
		"text":        text,
		"annotations": []any{},
	}
	message := map[string]any{
		"type":    "message",
		"id":      "msg_cached_0",
		"role":    "assistant",
		"status":  "completed",
		"content": []any{outputText},
	}
	return map[string]any{
		"id":                 id,
		"object":             "response",
		"created_at":         nowUnix,
		"status":             "completed",
		"model":              model,
		"output":             []any{message},
		"error":              nil,
		"incomplete_details": nil,
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": len([]rune(text)),
			"total_tokens":  len([]rune(text)),
		},
	}
}

func extractContentString(val any) string {
	if val == nil {
		return ""
	}
	if s, ok := val.(string); ok {
		return s
	}
	if arr, ok := val.([]any); ok {
		res := ""
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					res += t
				}
			}
		}
		return res
	}
	return fmt.Sprintf("%v", val)
}
