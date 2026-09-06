package probecache

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEngineInputHashAndLimits(t *testing.T) {
	store := NewStore("", 24*time.Hour, 3)
	eng := NewEngine(store, 20480, 200)

	// 1. 正常小文本输入 -> 应该符合条件 (eligible = true)
	bodySmall := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ping"}]}`)
	hash1, eligible1, isStream1, format1 := eng.ComputeInputHash(bodySmall)
	if !eligible1 || hash1 == "" || isStream1 || format1 != "chat_completions" {
		t.Fatalf("expected eligible chat request, got hash=%s eligible=%v stream=%v format=%s", hash1, eligible1, isStream1, format1)
	}

	// 相同内容带 stream: true -> 应产生相同 hash，但 isStream = true
	bodyStream := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	hash2, eligible2, isStream2, format2 := eng.ComputeInputHash(bodyStream)
	if !eligible2 || hash2 != hash1 || !isStream2 || format2 != format1 {
		t.Fatalf("expected same hash with stream=true, got hash=%s stream=%v", hash2, isStream2)
	}

	// 2. 超长输入 (> 20KB) -> 应该拒绝缓存 (eligible = false)
	longText := strings.Repeat("x", 21000)
	bodyLong := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"` + longText + `"}]}`)
	_, eligibleLong, _, _ := eng.ComputeInputHash(bodyLong)
	if eligibleLong {
		t.Fatalf("expected body > 20KB to be ineligible")
	}
}

func TestEngineOutputTextExtractionAndLimits(t *testing.T) {
	store := NewStore("", 24*time.Hour, 3)
	eng := NewEngine(store, 20480, 200)

	// 1. 简短输出 (<= 200 字符) -> eligible = true
	shortResp := []byte(`{
		"id": "chatcmpl-123",
		"object": "chat.completion",
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "pong!"
			}
		}]
	}`)
	text, eligible := eng.ExtractOutputText("chat_completions", shortResp)
	if !eligible || text != "pong!" {
		t.Fatalf("expected eligible output pong!, got text=%s eligible=%v", text, eligible)
	}

	// 2. 超长输出 (> 200 字符) -> eligible = false
	longText := strings.Repeat("a", 250)
	longResp := []byte(`{
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "` + longText + `"
			}
		}]
	}`)
	_, eligibleLong := eng.ExtractOutputText("chat_completions", longResp)
	if eligibleLong {
		t.Fatalf("expected output > 200 chars to be ineligible")
	}
}

func TestEngineFormatResponseStreamAndNonStream(t *testing.T) {
	store := NewStore("", 24*time.Hour, 3)
	eng := NewEngine(store, 20480, 200)

	sample := &Sample{
		Text: "Hello world",
		RawResponse: []byte(`{
			"id": "chatcmpl-orig",
			"object": "chat.completion",
			"model": "gpt-4o",
			"choices": [{"message": {"role": "assistant", "content": "Hello world"}}]
		}`),
	}

	// 非流式格式化
	respNonStream := eng.FormatResponse(sample, false, "chat_completions", "gpt-4o")
	if respNonStream == nil || !respNonStream.Terminate || respNonStream.StatusCode != 200 {
		t.Fatalf("expected 200 non-stream response")
	}
	if !strings.Contains(string(respNonStream.ResponseBody), "Hello world") {
		t.Errorf("response body does not contain sample text: %s", string(respNonStream.ResponseBody))
	}

	// 流式格式化 (SSE)
	respStream := eng.FormatResponse(sample, true, "chat_completions", "gpt-4o")
	if respStream == nil || !respStream.Terminate || respStream.StatusCode != 200 {
		t.Fatalf("expected 200 stream response")
	}
	if respStream.ResponseHeaders.Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", respStream.ResponseHeaders.Get("Content-Type"))
	}
	if !strings.Contains(string(respStream.ResponseBody), "data: [DONE]") {
		t.Errorf("stream body missing [DONE]: %s", string(respStream.ResponseBody))
	}
}

func TestHashIgnoresFormatAndStream(t *testing.T) {
	store := NewStore("", 24*time.Hour, 3)
	eng := NewEngine(store, 20480, 500)

	// ?? model+input,?? format/stream -> ??? hash
	bodyChat := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ping"}]}`)
	bodyChatStream := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	bodyResponses := []byte(`{"model":"gpt-4o","input":[{"role":"user","content":"ping"}]}`)

	h1, ok1, _, _ := eng.ComputeInputHash(bodyChat)
	h2, ok2, _, _ := eng.ComputeInputHash(bodyChatStream)
	h3, ok3, _, _ := eng.ComputeInputHash(bodyResponses)

	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("all three bodies should be eligible")
	}
	if h1 != h2 || h1 != h3 {
		t.Fatalf("hash must match on model+input only: chat=%s stream=%s responses=%s", h1[:12], h2[:12], h3[:12])
	}
}

func TestExtractStreamTextChatCompletions(t *testing.T) {
	store := NewStore("", 24*time.Hour, 3)
	eng := NewEngine(store, 20480, 500)

	chunks := [][]byte{
		[]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"),
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"),
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo!\"}}]}\n\n"),
		[]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}

	text, ok := eng.ExtractStreamText("chat_completions", chunks)
	if !ok || text != "Hello!" {
		t.Fatalf("expected Hello!, got %q ok=%v", text, ok)
	}
}

func TestExtractStreamTextResponses(t *testing.T) {
	store := NewStore("", 24*time.Hour, 3)
	eng := NewEngine(store, 20480, 500)

	chunks := [][]byte{
		[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"wor\"}\n\n"),
		[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ld\"}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}

	text, ok := eng.ExtractStreamText("responses", chunks)
	if !ok || text != "world" {
		t.Fatalf("expected world, got %q ok=%v", text, ok)
	}
}

func TestFormatResponseResponsesNonStream(t *testing.T) {
	store := NewStore("", 24*time.Hour, 3)
	eng := NewEngine(store, 20480, 500)

	sample := &Sample{Text: "pong", RawResponse: []byte("pong")}
	resp := eng.FormatResponse(sample, false, "responses", "gpt-5.6-terra")
	if resp == nil || !resp.Terminate || resp.StatusCode != 200 {
		t.Fatalf("expected 200 terminate response")
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp.ResponseBody, &parsed); err != nil {
		t.Fatalf("responses replay body must be valid JSON: %v", err)
	}
	if parsed["object"] != "response" {
		t.Fatalf("expected object=response, got %v", parsed["object"])
	}
	output, _ := parsed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 output item")
	}
	if !strings.Contains(string(resp.ResponseBody), "pong") {
		t.Fatalf("body missing sample text")
	}
}

func TestFormatResponseResponsesStream(t *testing.T) {
	store := NewStore("", 24*time.Hour, 3)
	eng := NewEngine(store, 20480, 500)

	sample := &Sample{Text: "pong", RawResponse: []byte("pong")}
	resp := eng.FormatResponse(sample, true, "responses", "gpt-5.6-terra")
	if resp == nil || !resp.Terminate {
		t.Fatalf("expected terminate response")
	}
	body := string(resp.ResponseBody)
	if !strings.Contains(body, "response.created") || !strings.Contains(body, "response.output_text.delta") || !strings.Contains(body, "response.completed") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("responses SSE missing required events: %s", body)
	}
}
