package probecache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

	// 计算哈希
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte("|"))
	h.Write([]byte(format))
	for _, p := range normalizedPrompts {
		h.Write([]byte("|"))
		h.Write([]byte(p))
	}

	return hex.EncodeToString(h.Sum(nil)), true, stream, format
}

// ExtractOutputText 从上游响应中提取纯文本输出并校验长度 <= 200 字符
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

// FormatResponse 构造缓存命中的返回响应（自适应流式 SSE 与非流式 JSON）
func (e *Engine) FormatResponse(sample *Sample, isStream bool, format, model string) *types.RequestInterceptResponse {
	if sample == nil {
		return nil
	}

	nowUnix := time.Now().Unix()
	respHeaders := make(http.Header)

	if !isStream {
		// 非流式：直接返回调整过 id 与时间的 JSON
		respHeaders.Set("Content-Type", "application/json")

		var parsed map[string]any
		if err := json.Unmarshal(sample.RawResponse, &parsed); err == nil {
			parsed["id"] = fmt.Sprintf("chatcmpl-cached-%d", time.Now().UnixNano())
			parsed["created"] = nowUnix
			body, _ := json.Marshal(parsed)
			return &types.RequestInterceptResponse{
				Terminate:       true,
				StatusCode:      200,
				ResponseBody:    body,
				ResponseHeaders: respHeaders,
			}
		}

		return &types.RequestInterceptResponse{
			Terminate:       true,
			StatusCode:      200,
			ResponseBody:    []byte(sample.RawResponse),
			ResponseHeaders: respHeaders,
		}
	}

	// 流式：将样本转换为标准 SSE 事件流
	respHeaders.Set("Content-Type", "text/event-stream")
	respHeaders.Set("Cache-Control", "no-cache")
	respHeaders.Set("Connection", "keep-alive")

	sseID := fmt.Sprintf("chatcmpl-cached-%d", time.Now().UnixNano())

	// 构造分块
	chunk1 := map[string]any{
		"id":      sseID,
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
		"id":      sseID,
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
		"id":      sseID,
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
