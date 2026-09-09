package guard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"cpa-codex-guard/internal/state"
	"cpa-codex-guard/internal/types"
)

var (
	signatureErrorRegex = regexp.MustCompile(`item\s+(rs_[a-zA-Z0-9]+)\s+could\s+not\s+be\s+verified`)
)

type Checker struct {
	store                  *state.Store
	maxPayloadBytes        int
	blockMaxTurns          bool
	blockInvalidSignatures bool
	autoFixResponsesLite   bool
	fuzzyCircuitBreaker    bool
	similarityThreshold    float64
}

func NewChecker(store *state.Store, maxPayloadBytes int, blockMaxTurns, blockInvalidSignatures, autoFixResponsesLite, fuzzyCircuitBreaker bool, threshold float64) *Checker {
	if threshold <= 0 || threshold > 1.0 {
		threshold = 0.90
	}
	return &Checker{
		store:                  store,
		maxPayloadBytes:        maxPayloadBytes,
		blockMaxTurns:          blockMaxTurns,
		blockInvalidSignatures: blockInvalidSignatures,
		autoFixResponsesLite:   autoFixResponsesLite,
		fuzzyCircuitBreaker:    fuzzyCircuitBreaker,
		similarityThreshold:    threshold,
	}
}

// CheckRequest 在 request.intercept_before 钩子中执行
func (c *Checker) CheckRequest(req *types.RequestInterceptRequest) *types.RequestInterceptResponse {
	if req == nil {
		return nil
	}
	body := req.GetRequestBody()
	if len(body) == 0 {
		return nil
	}

	// 0. 超大 Payload 防御：当请求体体积超过阈值时，在反序列化前直接 502 短路
	if c.maxPayloadBytes > 0 && len(body) > c.maxPayloadBytes {
		return shortCircuitOverloadedPayload(len(body), c.maxPayloadBytes)
	}

	headers := req.GetHeaders()

	hasMaxTurnsStr := c.blockMaxTurns && containsSubslice(body, []byte(`"max_turns"`))
	hasReasoningStr := c.blockInvalidSignatures && (containsSubslice(body, []byte(`"reasoning"`)) || containsSubslice(body, []byte(`"rs_`)))
	isResponsesLite := c.autoFixResponsesLite && isResponsesLiteHeader(headers)

	// 如果没有特殊匹配且未开熔断，快速放行
	if !hasMaxTurnsStr && !hasReasoningStr && !isResponsesLite && !c.fuzzyCircuitBreaker {
		return nil
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}

	// 1. 静态规则：max_turns 拦截
	if c.blockMaxTurns {
		if _, exists := root["max_turns"]; exists {
			return shortCircuitMaxTurns()
		}
	}

	// 提取会话 keys 与 input hash 清单
	sessionKeys := extractSessionKeys(req.Headers, root)
	inputHashes := extractInputHashes(root)

	// 2. 动态模糊熔断：若会话处于 3h 熔断且重合度 >= 90%，直接短路返回上次错误体
	if c.fuzzyCircuitBreaker && len(sessionKeys) > 0 && len(inputHashes) > 0 {
		if tripped, code, payload := c.store.CheckFuzzyCircuit(sessionKeys, inputHashes, c.similarityThreshold); tripped {
			if code == 0 {
				code = 400
			}
			if len(payload) == 0 {
				payload = []byte(`{"error":{"message":"Request temporarily circuit-broken after consecutive errors in session","type":"circuit_breaker_error"}}`)
			}
			headers := make(http.Header)
			headers.Set("Content-Type", "application/json")
			return &types.RequestInterceptResponse{
				Terminate:       true,
				StatusCode:      code,
				ResponseBody:    payload,
				ResponseHeaders: headers,
			}
		}
	}

	// 3. 动态规则：thinking_signature_invalid 拦截
	if c.blockInvalidSignatures {
		if inputRaw, exists := root["input"]; exists {
			var inputItems []map[string]json.RawMessage
			if err := json.Unmarshal(inputRaw, &inputItems); err == nil {
				for _, item := range inputItems {
					var itemID string
					if idRaw, ok := item["id"]; ok {
						_ = json.Unmarshal(idRaw, &itemID)
					}

					if itemID != "" {
						if isInvalid, recordedErr := c.store.IsInvalid(itemID); isInvalid {
							return shortCircuitInvalidSignature(recordedErr)
						}
					}
				}
			}
		}
	}

	// 4. 自动修复：Responses-Lite 缺失 reasoning.context
	if isResponsesLite {
		needFix := false
		reasoningRaw, exists := root["reasoning"]
		if !exists || len(reasoningRaw) == 0 || string(reasoningRaw) == "null" {
			needFix = true
			root["reasoning"] = json.RawMessage(`{"context":"all_turns"}`)
		} else {
			var reasoningMap map[string]any
			if err := json.Unmarshal(reasoningRaw, &reasoningMap); err == nil {
				if ctxVal, ok := reasoningMap["context"]; !ok || ctxVal != "all_turns" {
					needFix = true
					reasoningMap["context"] = "all_turns"
					fixedReasoningBytes, _ := json.Marshal(reasoningMap)
					root["reasoning"] = json.RawMessage(fixedReasoningBytes)
				}
			}
		}

		if needFix {
			fixedBody, err := json.Marshal(root)
			if err == nil {
				return &types.RequestInterceptResponse{
					Terminate: false,
					Body:      fixedBody,
				}
			}
		}
	}

	return nil
}

// ObserveResponse 在 request.intercept_after / response.intercept_after 钩子中执行
func (c *Checker) ObserveResponse(req *types.RequestInterceptRequest) {
	if req == nil {
		return
	}

	reqBody := req.GetRequestBody()
	respBody := req.GetResponseBody()
	headers := req.GetHeaders()

	var root map[string]json.RawMessage
	if len(reqBody) > 0 {
		_ = json.Unmarshal(reqBody, &root)
	}

	sessionKeys := extractSessionKeys(headers, root)
	inputHashes := extractInputHashes(root)

	c.RecordOutcome(sessionKeys, inputHashes, req.StatusCode, respBody)
}

// RecordOutcome 由 engine 在响应钩子或 request.complete 时调用，统一记录
// 熔断结果并捕获 thinking_signature_invalid。statusCode==0（上游执行前的
// 钩子）会被 state.Store 忽略。
func (c *Checker) RecordOutcome(sessionKeys, inputHashes []string, statusCode int, respBody []byte) {
	// 1. 记录模糊熔断结果（无论是成功 200 重置，还是连续失败累计）
	if c.fuzzyCircuitBreaker && len(sessionKeys) > 0 {
		c.store.RecordResponseOutcome(sessionKeys, inputHashes, statusCode, respBody, c.similarityThreshold)
	}

	// 2. 捕获 thinking_signature_invalid 并持久化记录失效 itemID
	if statusCode == 400 && len(respBody) > 0 {
		respBodyStr := string(respBody)
		matches := signatureErrorRegex.FindStringSubmatch(respBodyStr)
		if len(matches) >= 2 {
			failedItemID := matches[1]
			sess := ""
			if len(sessionKeys) > 0 {
				sess = sessionKeys[0]
			}
			c.store.MarkInvalid(failedItemID, sess, respBody)
		}
	}
}

// ExtractSessionKeys 导出给 engine 在 intercept_before / stream header-init
// 时预提取会话键（供 request.complete 流式失败观察使用）。
func ExtractSessionKeys(headers http.Header, root map[string]json.RawMessage) []string {
	return extractSessionKeys(headers, root)
}

// ExtractInputHashes 导出给 engine 预提取 input 哈希清单。
func ExtractInputHashes(root map[string]json.RawMessage) []string {
	return extractInputHashes(root)
}

func extractSessionKeys(headers http.Header, root map[string]json.RawMessage) []string {
	keys := make([]string, 0, 4)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" {
			for _, existing := range keys {
				if existing == v {
					return
				}
			}
			keys = append(keys, v)
		}
	}

	// Header
	if headers != nil {
		add(headers.Get("Session-Id"))
		add(headers.Get("X-Codex-Window-Id"))
	}

	// Client metadata
	if root != nil {
		if metaRaw, ok := root["client_metadata"]; ok && len(metaRaw) > 0 {
			var meta map[string]any
			if err := json.Unmarshal(metaRaw, &meta); err == nil {
				if s, ok := meta["session_id"].(string); ok {
					add(s)
				}
				if r, ok := meta["root_turn_id"].(string); ok {
					add(r)
				}
				if w, ok := meta["x-codex-window-id"].(string); ok {
					add(w)
				}
			}
		}
	}

	return keys
}

func extractInputHashes(root map[string]json.RawMessage) []string {
	if root == nil {
		return nil
	}
	inputRaw, exists := root["input"]
	if !exists || len(inputRaw) == 0 {
		return nil
	}

	var items []any
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return nil
	}

	hashes := make([]string, 0, len(items))
	for _, it := range items {
		hashes = append(hashes, state.HashJSON(it))
	}
	return hashes
}

func isResponsesLiteHeader(headers http.Header) bool {
	if headers == nil {
		return false
	}
	for k, v := range headers {
		if strings.EqualFold(k, "X-OpenAI-Internal-Codex-Responses-Lite") {
			for _, val := range v {
				if strings.EqualFold(val, "true") {
					return true
				}
			}
		}
	}
	return false
}

func shortCircuitMaxTurns() *types.RequestInterceptResponse {
	respBody := []byte(`{"error":{"message":"Bad Request","type":"invalid_request_error"}}`)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return &types.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      400,
		ResponseBody:    respBody,
		ResponseHeaders: headers,
	}
}

func shortCircuitOverloadedPayload(bodyLen, maxBytes int) *types.RequestInterceptResponse {
	respBody := []byte(fmt.Sprintf(`{"error":{"message":"server_is_overloaded: payload size (%d bytes) exceeds maximum limit (%d bytes)","type":"server_overloaded_error"}}`, bodyLen, maxBytes))
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return &types.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      http.StatusBadGateway, // 502
		ResponseBody:    respBody,
		ResponseHeaders: headers,
	}
}

func shortCircuitInvalidSignature(payload []byte) *types.RequestInterceptResponse {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return &types.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      400,
		ResponseBody:    payload,
		ResponseHeaders: headers,
	}
}

func containsSubslice(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
