package guard

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"cpa-codex-guard/internal/state"
	"cpa-codex-guard/internal/types"
)

func TestCheckMaxTurns(t *testing.T) {
	store := state.NewStore("", 24*time.Hour, 3*time.Hour)
	checker := NewChecker(store, 15*1024*1024, true, true, true, true, 0.90)

	bodyWithMaxTurns := []byte(`{"model":"gpt-5.6-luna","max_turns":3,"input":[]}`)
	req := &types.RequestInterceptRequest{
		Body: bodyWithMaxTurns,
	}

	resp := checker.CheckRequest(req)
	if resp == nil || !resp.Terminate {
		t.Fatalf("expected request with max_turns to terminate")
	}
	if resp.StatusCode != 400 {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	var errMap map[string]any
	if err := json.Unmarshal(resp.ResponseBody, &errMap); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	errObj := errMap["error"].(map[string]any)
	if errObj["message"] != "Bad Request" {
		t.Errorf("unexpected message: %v", errObj["message"])
	}
}

func TestCheckInvalidSignature(t *testing.T) {
	store := state.NewStore("", 24*time.Hour, 3*time.Hour)
	checker := NewChecker(store, 15*1024*1024, true, true, true, true, 0.90)

	mockUpstreamErr := []byte(`{"error":{"code":"thinking_signature_invalid","message":"The encrypted content for item rs_08ccb93bdcdfbad6016a9b91fc9f388195b666d5e75bc3d65f could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error"}}`)

	reqAfter := &types.RequestInterceptRequest{
		StatusCode:   400,
		ResponseBody: mockUpstreamErr,
		Headers:      map[string][]string{"Session-Id": {"test-session-1"}},
	}
	checker.ObserveResponse(reqAfter)

	reqBefore := &types.RequestInterceptRequest{
		Body: []byte(`{
			"model": "gpt-5.6-luna",
			"input": [
				{"role": "user", "content": "hello"},
				{"type": "reasoning", "id": "rs_08ccb93bdcdfbad6016a9b91fc9f388195b666d5e75bc3d65f"}
			]
		}`),
	}

	resp := checker.CheckRequest(reqBefore)
	if resp == nil || !resp.Terminate {
		t.Fatalf("expected request with invalidated signature to terminate")
	}
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	if string(resp.ResponseBody) != string(mockUpstreamErr) {
		t.Errorf("expected response to match recorded upstream error")
	}

	reqNormal := &types.RequestInterceptRequest{
		Body: []byte(`{
			"model": "gpt-5.6-luna",
			"input": [
				{"role": "user", "content": "hello"},
				{"type": "reasoning", "id": "rs_other_good_item"}
			]
		}`),
	}
	if respNormal := checker.CheckRequest(reqNormal); respNormal != nil {
		t.Errorf("expected normal request to pass through")
	}
}

func TestAutoFixResponsesLiteReasoning(t *testing.T) {
	store := state.NewStore("", 24*time.Hour, 3*time.Hour)
	checker := NewChecker(store, 15*1024*1024, true, true, true, true, 0.90)

	headers := make(http.Header)
	headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")

	// 1. 无 reasoning 字段，期望自动注入 reasoning.context = "all_turns"
	reqNoReasoning := &types.RequestInterceptRequest{
		Headers: headers,
		Body:    []byte(`{"model":"gpt-6-astra","input":[]}`),
	}
	resp1 := checker.CheckRequest(reqNoReasoning)
	if resp1 == nil {
		t.Fatalf("expected resp with modified body, got nil")
	}
	if resp1.Terminate {
		t.Errorf("expected Terminate=false for auto-fix")
	}
	var resMap1 map[string]any
	if err := json.Unmarshal(resp1.Body, &resMap1); err != nil {
		t.Fatalf("failed to unmarshal fixed body: %v", err)
	}
	reasoning1, ok := resMap1["reasoning"].(map[string]any)
	if !ok || reasoning1["context"] != "all_turns" {
		t.Errorf("expected reasoning.context == all_turns, got: %v", resMap1["reasoning"])
	}

	// 2. 已有 reasoning 且 context 已经是 all_turns，应放行不需修改
	reqValidReasoning := &types.RequestInterceptRequest{
		Headers: headers,
		Body:    []byte(`{"model":"gpt-6-astra","reasoning":{"context":"all_turns"},"input":[]}`),
	}
	resp2 := checker.CheckRequest(reqValidReasoning)
	if resp2 != nil {
		t.Errorf("expected nil resp for already valid request, got: %v", resp2)
	}
}

func TestFuzzyCircuitBreakerTripped(t *testing.T) {
	store := state.NewStore("", 24*time.Hour, 3*time.Hour)
	checker := NewChecker(store, 15*1024*1024, true, true, true, true, 0.90)

	// 构造 10 条 input 的初始请求
	baseBody := `{"client_metadata":{"session_id":"sess-999"},"input":[`
	for i := 0; i < 10; i++ {
		if i > 0 {
			baseBody += ","
		}
		baseBody += `{"role":"user","content":"line"}`
	}
	baseBody += `]}`

	mockErr := []byte(`{"error":{"type":"overloaded_error","message":"rate limited upstream"}}`)

	// 第 1 次失败
	checker.ObserveResponse(&types.RequestInterceptRequest{
		StatusCode:   429,
		Body:         []byte(baseBody),
		ResponseBody: mockErr,
	})

	// 此时发起第 2 次请求（相似度 100%），尚未被熔断，正常放行
	resp := checker.CheckRequest(&types.RequestInterceptRequest{
		Body: []byte(baseBody),
	})
	if resp != nil {
		t.Fatalf("expected 2nd request to not be blocked before 2nd error recorded")
	}

	// 第 2 次再次失败
	checker.ObserveResponse(&types.RequestInterceptRequest{
		StatusCode:   429,
		Body:         []byte(baseBody),
		ResponseBody: mockErr,
	})

	// 此时发起第 3 次请求（相似度 100% >= 90%），应当立即被短路拦截！
	trippedResp := checker.CheckRequest(&types.RequestInterceptRequest{
		Body: []byte(baseBody),
	})
	if trippedResp == nil || !trippedResp.Terminate {
		t.Fatalf("expected 3rd request to be short-circuited by fuzzy circuit breaker")
	}
	if trippedResp.StatusCode != 429 {
		t.Errorf("expected status 429, got %d", trippedResp.StatusCode)
	}
	if string(trippedResp.ResponseBody) != string(mockErr) {
		t.Errorf("expected error body to match upstream, got %s", string(trippedResp.ResponseBody))
	}
}

func TestCheckMaxPayloadBytes(t *testing.T) {
	store := state.NewStore("", 24*time.Hour, 3*time.Hour)
	checker := NewChecker(store, 100, true, true, true, true, 0.90)

	smallBody := []byte(`{"model":"gpt-5.6-luna","input":[{"role":"user","content":"hi"}]}`)
	respSmall := checker.CheckRequest(&types.RequestInterceptRequest{Body: smallBody})
	if respSmall != nil {
		t.Fatalf("expected small request to pass, got %+v", respSmall)
	}

	largeBody := make([]byte, 101)
	for i := range largeBody {
		largeBody[i] = 'a'
	}
	respLarge := checker.CheckRequest(&types.RequestInterceptRequest{Body: largeBody})
	if respLarge == nil || !respLarge.Terminate {
		t.Fatalf("expected large request to terminate")
	}
	if respLarge.StatusCode != http.StatusBadGateway {
		t.Errorf("expected status %d (502), got %d", http.StatusBadGateway, respLarge.StatusCode)
	}
	var errResp map[string]any
	if err := json.Unmarshal(respLarge.ResponseBody, &errResp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}
	errObj := errResp["error"].(map[string]any)
	if errObj["type"] != "server_overloaded_error" {
		t.Errorf("expected server_overloaded_error, got %v", errObj["type"])
	}
}
