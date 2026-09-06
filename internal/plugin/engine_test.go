package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"cpa-codex-guard/internal/types"
)

func TestEngineRegister(t *testing.T) {
	engine := newTestEngine(t)
	regBytes, err := engine.HandleMethod(types.MethodPluginRegister, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	var env types.Envelope
	if err := json.Unmarshal(regBytes, &env); err != nil {
		t.Fatalf("unmarshal env failed: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected env.OK=true")
	}

	var reg types.Registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("unmarshal reg failed: %v", err)
	}

	if !reg.Capabilities.RequestInterceptor {
		t.Errorf("expected RequestInterceptor to be true")
	}
	if !reg.Capabilities.RequestLifecyclePlugin {
		t.Errorf("expected RequestLifecyclePlugin to be true")
	}
	if reg.Metadata.Name != types.PluginID {
		t.Errorf("unexpected name: %s", reg.Metadata.Name)
	}
}

// request.intercept_after must NOT collect samples: its Body is the rewritten
// upstream request, not a response, and success is unknown at that point.
// newTestEngine builds an Engine with isolated state files so tests never
// touch the production probe-cache/guard state under data/.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.StateFile = dir + "/guard-state.json"
	cfg.ProbeStateFile = dir + "/probe-state.json"
	return newEngineWithConfig(cfg)
}

func TestRequestInterceptAfterDoesNotCollectSample(t *testing.T) {
	engine := newTestEngine(t)
	body := []byte(`{"model":"m-test","messages":[{"role":"user","content":"ping"}]}`)
	raw := mustRequestAfterAuth(t, body)
	if _, err := engine.HandleMethod(types.MethodRequestInterceptAfter, raw); err != nil {
		t.Fatalf("request.intercept_after failed: %v", err)
	}

	hash, eligible, _, _ := engine.probeEngine.ComputeInputHash(body)
	if !eligible || hash == "" {
		t.Fatalf("expected eligible hash for body")
	}
	if _, hit := engine.probeEngine.Store().GetRandomSample(hash); hit {
		t.Fatalf("request.intercept_after must not create probe cache samples")
	}
}

// response.intercept_after is the authoritative collection point: a real 200
// response body becomes a sample, and the pending entry for that RequestID is
// consumed.
func TestResponseInterceptAfterCollectsRealSample(t *testing.T) {
	engine := newTestEngine(t)
	reqBody := []byte(`{"model":"m-test","messages":[{"role":"user","content":"ping"}]}`)
	respBody := []byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`)

	// Seed a pending entry as intercept_before would.
	hash, eligible, _, _ := engine.probeEngine.ComputeInputHash(reqBody)
	if !eligible || hash == "" {
		t.Fatalf("expected eligible hash")
	}
	engine.pending["req-1"] = &pendingInfo{hash: hash, model: "m-test", format: "chat_completions", seenAt: time.Now()}

	raw := mustResponseIntercept(t, reqBody, respBody)
	if _, err := engine.HandleMethod(types.MethodResponseInterceptAfter, raw); err != nil {
		t.Fatalf("response.intercept_after failed: %v", err)
	}

	samples, ok := engine.probeEngine.Store().PeekSamples(hash)
	if !ok || len(samples) == 0 {
		t.Fatalf("expected probe cache sample from real response")
	}
	if samples[0].Text != "pong" {
		t.Fatalf("expected sample text 'pong', got %q", samples[0].Text)
	}

	engine.pendingMu.Lock()
	_, stillPending := engine.pending["req-1"]
	engine.pendingMu.Unlock()
	if stillPending {
		t.Fatalf("pending entry should be consumed by response hook")
	}
}

// A failing request never reaches response.intercept_after; its pending entry
// must be evicted by request.complete so the map cannot leak.
func TestRequestCompleteEvictsPending(t *testing.T) {
	engine := newTestEngine(t)
	reqBody := []byte(`{"model":"m-test","messages":[{"role":"user","content":"ping"}]}`)
	hash, eligible, _, _ := engine.probeEngine.ComputeInputHash(reqBody)
	if !eligible || hash == "" {
		t.Fatalf("expected eligible hash")
	}
	engine.pending["req-429"] = &pendingInfo{hash: hash, model: "m-test", format: "chat_completions", seenAt: time.Now()}

	completion := types.RequestCompletion{
		RequestID:  "req-429",
		Outcome:    "failed",
		StatusCode: 429,
	}
	raw, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	if _, err := engine.HandleMethod(types.MethodRequestComplete, raw); err != nil {
		t.Fatalf("request.complete failed: %v", err)
	}

	engine.pendingMu.Lock()
	_, stillPending := engine.pending["req-429"]
	engine.pendingMu.Unlock()
	if stillPending {
		t.Fatalf("pending entry should be evicted by request.complete")
	}

	if samples, ok := engine.probeEngine.Store().PeekSamples(hash); ok && len(samples) > 0 {
		t.Fatalf("failed request must not create probe cache samples")
	}
}

// mustRequestAfterAuth mirrors the host payload for request.intercept_after
// (Body = rewritten upstream request, no status/response fields).
func mustRequestAfterAuth(t *testing.T, body []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(types.RequestInterceptRequest{
		RequestID:    "req-1",
		SourceFormat: "openai",
		ToFormat:     "openai",
		Model:        "m-test",
		Stream:       false,
		Body:         body,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return raw
}

// mustResponseIntercept mirrors the host payload for response.intercept_after
// (RequestBody = upstream request, Body = response body, StatusCode = 200).
func mustResponseIntercept(t *testing.T, reqBody, respBody []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(types.RequestInterceptRequest{
		RequestID:    "req-1",
		SourceFormat: "openai",
		Model:        "m-test",
		RequestBody:  reqBody,
		Body:         respBody,
		StatusCode:   200,
	})
	if err != nil {
		t.Fatalf("marshal response intercept: %v", err)
	}
	return raw
}

// Streaming: header-init seeds pending, payload chunks accumulate SSE text,
// request.complete finalizes the sample. Regression test for the strings.
// Builder copy panic (map value copy) that crashed the host process.
func TestStreamChunkCollectsSampleOnComplete(t *testing.T) {
	engine := newTestEngine(t)
	reqBody := []byte(`{"model":"m-test","stream":true,"messages":[{"role":"user","content":"ping"}]}`)

	initRaw, err := json.Marshal(types.StreamChunkInterceptRequest{
		RequestID:    "req-stream-1",
		SourceFormat: "openai",
		Model:        "m-test",
		RequestBody:  reqBody,
		ChunkIndex:   types.StreamChunkHeaderInitIndex,
	})
	if err != nil {
		t.Fatalf("marshal init chunk: %v", err)
	}
	if _, err := engine.HandleMethod(types.MethodResponseInterceptStreamChunk, initRaw); err != nil {
		t.Fatalf("stream header-init failed: %v", err)
	}

	for _, frame := range []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"He"}}]}`,
		`data: {"choices":[{"delta":{"content":"llo"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	} {
		raw, errMarshal := json.Marshal(types.StreamChunkInterceptRequest{
			RequestID:  "req-stream-1",
			Model:      "m-test",
			Body:       []byte(frame),
			ChunkIndex: 1,
		})
		if errMarshal != nil {
			t.Fatalf("marshal chunk: %v", errMarshal)
		}
		if _, err := engine.HandleMethod(types.MethodResponseInterceptStreamChunk, raw); err != nil {
			t.Fatalf("stream chunk failed: %v", err)
		}
	}

	hash, _, _, _ := engine.probeEngine.ComputeInputHash(reqBody)
	completionRaw, err := json.Marshal(types.RequestCompletion{
		RequestID:  "req-stream-1",
		Outcome:    types.OutcomeSucceeded,
		StatusCode: 200,
		Stream:     true,
	})
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	if _, err := engine.HandleMethod(types.MethodRequestComplete, completionRaw); err != nil {
		t.Fatalf("request.complete failed: %v", err)
	}

	samples, ok := engine.probeEngine.Store().PeekSamples(hash)
	if !ok || len(samples) == 0 {
		t.Fatalf("expected stream sample collected on request.complete")
	}
	if samples[0].Text != "Hello" {
		t.Fatalf("expected stream sample text Hello, got %q", samples[0].Text)
	}

	engine.pendingMu.Lock()
	_, stillPending := engine.pending["req-stream-1"]
	engine.pendingMu.Unlock()
	if stillPending {
		t.Fatalf("pending entry should be evicted after completion")
	}
}
