package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cpa-codex-guard/internal/guard"
	"cpa-codex-guard/internal/probecache"
	"cpa-codex-guard/internal/state"
	"cpa-codex-guard/internal/types"
)

// debugLog is temporary diagnostic instrumentation (kept per user request).
// Relative "data/..." on purpose: resolves against the CPA process working
// directory, keeping the plugin portable across platforms/installs.
func (e *Engine) debugLog(stage, msg string) {
	f, err := os.OpenFile(filepath.Join("data", "cpa-codex-guard-debug.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(time.Now().Format(time.RFC3339Nano) + " [" + stage + "] " + msg + "\n")
}

type Engine struct {
	mu          sync.RWMutex
	cfg         *Config
	store       *state.Store
	checker     *guard.Checker
	probeEngine *probecache.Engine

	pendingMu sync.Mutex
	pending   map[string]*pendingInfo
}

type pendingInfo struct {
	hash        string
	model       string
	format      string
	isStream    bool
	seenAt      time.Time
	sessionKeys []string
	inputHashes []string
	// streamText accumulates SSE delta text while the stream is in flight.
	streamText strings.Builder
	// chunkCount tracks how many SSE data frames were seen (diagnostics).
	chunkCount int
}

func NewEngine() *Engine {
	return newEngineWithConfig(DefaultConfig())
}

func newEngineWithConfig(cfg *Config) *Engine {
	store := state.NewStore(cfg.StateFile, cfg.ParseTTL(), cfg.ParseCircuitTTL())
	checker := guard.NewChecker(
		store,
		cfg.IsBlockMaxTurns(),
		cfg.IsBlockInvalidSignatures(),
		cfg.IsAutoFixResponsesLite(),
		cfg.IsFuzzyCircuitBreaker(),
		cfg.GetSimilarityThreshold(),
	)

	probeStore := probecache.NewStore(cfg.ProbeStateFile, cfg.ParseProbeTTL(), cfg.ProbeMinSamples)
	probeEngine := probecache.NewEngine(probeStore, cfg.ProbeMaxInputBytes, cfg.ProbeMaxOutputChars)

	e := &Engine{
		cfg:         cfg,
		store:       store,
		checker:     checker,
		probeEngine: probeEngine,
		pending:     make(map[string]*pendingInfo),
	}
	go e.pendingJanitor()
	return e
}

// pendingJanitor evicts stale pending entries whose request.complete never
// arrived (host restart, fused plugin, lifecycle delivery failure).
func (e *Engine) pendingJanitor() {
	ticker := time.NewTicker(10 * time.Minute)
	for range ticker.C {
		cutoff := time.Now().Add(-1 * time.Hour)
		e.pendingMu.Lock()
		for k, v := range e.pending {
			if v.seenAt.Before(cutoff) {
				delete(e.pending, k)
			}
		}
		e.pendingMu.Unlock()
	}
}

func (e *Engine) HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case types.MethodPluginRegister, types.MethodPluginReconfigure:
		return e.handleRegister(request)
	case types.MethodRequestInterceptBefore:
		return e.handleInterceptBefore(request)
	case types.MethodRequestInterceptAfter:
		return e.handleRequestInterceptAfter(request)
	case types.MethodResponseInterceptAfter:
		return e.handleResponseInterceptAfter(request)
	case types.MethodResponseInterceptStreamChunk:
		return e.handleStreamChunk(request)
	case types.MethodRequestComplete:
		return e.handleRequestComplete(request)
	default:
		return json.Marshal(types.Envelope{
			OK: false,
			Error: &types.EnvelopeError{
				Code:       "unknown_method",
				Message:    "unknown method: " + method,
				HTTPStatus: http.StatusNotFound,
			},
		})
	}
}

func (e *Engine) handleRegister(request []byte) ([]byte, error) {
	reg := types.Registration{
		SchemaVersion: types.SchemaVersion,
		Metadata: types.Metadata{
			Name:             types.PluginID,
			Version:          types.Version,
			Author:           "OpenCode",
			Description:      "Guards against max_turns, invalid signatures, auto-fixes Responses-Lite context, fuzzy circuit breaks, and caches probe traffic",
			GitHubRepository: "https://github.com/cwhypt/cliproxyapi",
			ConfigFields: []types.ConfigField{
				{Name: "state_file", Type: "string", Description: "Path to state persistence file"},
				{Name: "ttl", Type: "string", Description: "TTL for invalid signature cache"},
				{Name: "circuit_ttl", Type: "string", Description: "TTL for fuzzy circuit breaker (default 3h)"},
				{Name: "block_max_turns", Type: "boolean", Description: "Block requests with max_turns parameter"},
				{Name: "block_invalid_signatures", Type: "boolean", Description: "Block requests with known invalid thinking signatures"},
				{Name: "autofix_responses_lite", Type: "boolean", Description: "Auto-fix missing reasoning.context for Responses-Lite requests"},
				{Name: "fuzzy_circuit_breaker", Type: "boolean", Description: "Enable session-level fuzzy similarity circuit breaker"},
				{Name: "probe_cache_enabled", Type: "boolean", Description: "Enable caching and randomized playback for probe traffic"},
			},
		},
		Capabilities: types.Capabilities{
			RequestInterceptor:     true,
			ResponseInterceptor:    true,
			RequestLifecyclePlugin: true,
			StreamChunkInterceptor: true,
		},
	}

	resultBytes, _ := json.Marshal(reg)
	return json.Marshal(types.Envelope{
		OK:     true,
		Result: resultBytes,
	})
}

func (e *Engine) handleInterceptBefore(request []byte) ([]byte, error) {
	var req types.RequestInterceptRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return e.okEmptyResponse()
		}
	}

	e.mu.RLock()
	checker := e.checker
	probeEng := e.probeEngine
	cfg := e.cfg
	e.mu.RUnlock()

	res := checker.CheckRequest(&req)
	if res != nil {
		resBytes, err := json.Marshal(res)
		if err != nil {
			return e.okEmptyResponse()
		}
		return json.Marshal(types.Envelope{
			OK:     true,
			Result: resBytes,
		})
	}

	reqBody := req.GetRequestBody()
	if cfg.IsProbeCacheEnabled() && probeEng != nil && len(reqBody) > 0 {
		hash, eligible, isStream, format := probeEng.ComputeInputHash(reqBody)
		e.debugLog("before", fmt.Sprintf("reqId=%s model=%s stream=%v bodyLen=%d eligible=%v hash=%s format=%s", req.RequestID, req.Model, req.Stream, len(reqBody), eligible, hash[:min(len(hash), 12)], format))
		if eligible && hash != "" {
			if sample, hit := probeEng.Store().GetRandomSample(hash); hit {
				e.debugLog("before", fmt.Sprintf("CACHE_HIT hash=%s reqId=%s", hash, req.RequestID))
				probeResp := probeEng.FormatResponse(sample, isStream, format, req.Model)
				if probeResp != nil {
					resBytes, err := json.Marshal(probeResp)
					if err == nil {
						return json.Marshal(types.Envelope{
							OK:     true,
							Result: resBytes,
						})
					}
				}
			}
			if req.RequestID != "" {
				// Track session keys / input hashes so request.complete can still
				// observe failures for streaming requests (which never reach the
				// response interceptor).
				root := map[string]json.RawMessage{}
				_ = json.Unmarshal(reqBody, &root)
				e.pendingMu.Lock()
				if _, ok := e.pending[req.RequestID]; !ok {
					e.pending[req.RequestID] = &pendingInfo{
						hash:        hash,
						model:       req.Model,
						format:      format,
						isStream:    isStream,
						seenAt:      time.Now(),
						sessionKeys: guard.ExtractSessionKeys(req.GetHeaders(), root),
						inputHashes: guard.ExtractInputHashes(root),
					}
				}
				e.pendingMu.Unlock()
			}
		}
	}

	return e.okEmptyResponse()
}

// handleRequestInterceptAfter fires after credential selection, before upstream
// execution. Its Body is the rewritten upstream payload — NOT a response. There
// is no response data available here, so nothing is observed or recorded.
func (e *Engine) handleRequestInterceptAfter(request []byte) ([]byte, error) {
	return e.okEmptyResponse()
}

// handleResponseInterceptAfter fires only for successful non-streaming
// upstream responses (StatusCode=200). Body is the upstream response body and
// RequestBody is the exact payload sent upstream — this is the authoritative
// sample collection point for non-streaming traffic.
func (e *Engine) handleResponseInterceptAfter(request []byte) ([]byte, error) {
	var req types.RequestInterceptRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return e.okEmptyResponse()
		}
	}

	e.mu.RLock()
	checker := e.checker
	probeEng := e.probeEngine
	cfg := e.cfg
	e.mu.RUnlock()

	checker.ObserveResponse(&req)

	if cfg.IsProbeCacheEnabled() && probeEng != nil && req.StatusCode >= 200 && req.StatusCode < 300 {
		// Hash 必须基于客户端原始请求（OriginalRequest），与 intercept_before
		// 的命中查询保持同一基准；上游 payload 可能被转换/注入额外字段。
		reqBody := req.OriginalRequest
		if len(reqBody) == 0 {
			reqBody = req.GetRequestBody()
		}
		respBody := req.GetResponseBody()
		if len(reqBody) > 0 && len(respBody) > 0 {
			hash, eligible, _, format := probeEng.ComputeInputHash(reqBody)
			text, textOK := probeEng.ExtractOutputText(format, respBody)
			e.debugLog("collect", fmt.Sprintf("reqId=%s status=%d reqBodyLen=%d respBodyLen=%d hash=%s eligible=%v format=%s textOK=%v textLen=%d", req.RequestID, req.StatusCode, len(reqBody), len(respBody), hash[:min(len(hash), 12)], eligible, format, textOK, len([]rune(text))))
			if eligible && hash != "" {
				if textOK {
					probeEng.Store().AddSample(hash, req.Model, format, text, respBody)
					if err := probeEng.Store().LastError(); err != nil {
						e.debugLog("collect", fmt.Sprintf("PERSIST_ERROR reqId=%s store=%s err=%v", req.RequestID, "probe", err))
					}
					e.debugLog("collect", fmt.Sprintf("sample hash=%s model=%s textLen=%d reqId=%s", hash, req.Model, len([]rune(text)), req.RequestID))
				}
			}
		} else {
			e.debugLog("collect", fmt.Sprintf("reqId=%s status=%d EMPTY body reqBodyLen=%d respBodyLen=%d", req.RequestID, req.StatusCode, len(reqBody), len(respBody)))
		}
	}

	if req.RequestID != "" {
		e.pendingMu.Lock()
		delete(e.pending, req.RequestID)
		e.pendingMu.Unlock()
	}

	return e.okEmptyResponse()
}

// handleRequestComplete is the terminal lifecycle event for every request,
// success or failure. For streaming requests it is the only point where the
// final status is known, so failure recording and stream sample collection
// both happen here. Success cleanup for non-streaming normally happens in the
// response hook.
func (e *Engine) handleRequestComplete(request []byte) ([]byte, error) {
	var completion types.RequestCompletion
	if len(request) > 0 {
		if err := json.Unmarshal(request, &completion); err != nil {
			return e.okEmptyResponse()
		}
	}

	e.debugLog("complete", fmt.Sprintf("reqId=%s outcome=%s status=%d err=%s", completion.RequestID, completion.Outcome, completion.StatusCode, completion.Error))

	if completion.RequestID == "" {
		return e.okEmptyResponse()
	}

	e.pendingMu.Lock()
	p, hasPending := e.pending[reqKey(completion.RequestID)]
	delete(e.pending, completion.RequestID)
	e.pendingMu.Unlock()

	if !hasPending {
		return e.okEmptyResponse()
	}

	e.mu.RLock()
	checker := e.checker
	e.mu.RUnlock()

	// request.complete 是流式请求唯一能观察到最终状态的钩子；同时兜底
	// 非流式失败（response.intercept_after 只对成功响应触发）。
	switch {
	case completion.Outcome == types.OutcomeSucceeded && completion.StatusCode >= 200 && completion.StatusCode < 300:
		checker.RecordOutcome(p.sessionKeys, p.inputHashes, completion.StatusCode, nil)
		if p.isStream {
			e.collectStreamSample(completion.RequestID, p)
		}
	case completion.Outcome == types.OutcomeFailed && completion.StatusCode != 0:
		checker.RecordOutcome(p.sessionKeys, p.inputHashes, completion.StatusCode, []byte(completion.Error))
	}

	return e.okEmptyResponse()
}

// reqKey normalizes the pending-map key.
func reqKey(id string) string { return id }

// handleStreamChunk observes successful streaming responses. ChunkIndex ==
// StreamChunkHeaderInitIndex carries the request body (schema v3); payload
// chunks carry the SSE frame. Samples are collected on request.complete using
// the accumulated text, keyed by the pending map entry.
func (e *Engine) handleStreamChunk(request []byte) ([]byte, error) {
	var req types.StreamChunkInterceptRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return e.okStreamResponse(nil, false)
		}
	}

	e.mu.RLock()
	probeEng := e.probeEngine
	cfg := e.cfg
	e.mu.RUnlock()

	if req.ChunkIndex == types.StreamChunkHeaderInitIndex {
		// Header-init: ensure a pending entry exists even if intercept_before
		// skipped it (e.g. probe cache disabled then re-enabled mid-flight).
		if cfg.IsProbeCacheEnabled() && probeEng != nil && len(req.RequestBody) > 0 {
			hash, eligible, isStream, format := probeEng.ComputeInputHash(req.RequestBody)
			e.debugLog("stream-init", fmt.Sprintf("reqId=%s eligible=%v hash=%s format=%s", req.RequestID, eligible, hash[:min(len(hash), 12)], format))
			if eligible && hash != "" {
				e.pendingMu.Lock()
				if _, ok := e.pending[req.RequestID]; !ok {
					root := map[string]json.RawMessage{}
					_ = json.Unmarshal(req.RequestBody, &root)
					e.pending[req.RequestID] = &pendingInfo{
						hash:        hash,
						model:       req.Model,
						format:      format,
						isStream:    isStream,
						seenAt:      time.Now(),
						sessionKeys: guard.ExtractSessionKeys(req.RequestHeaders, root),
						inputHashes: guard.ExtractInputHashes(root),
					}
				}
				e.pendingMu.Unlock()
			}
		}
		return e.okStreamResponse(nil, false)
	}

	// Payload chunk: accumulate SSE delta text into the pending entry.
	if cfg.IsProbeCacheEnabled() && probeEng != nil && len(req.Body) > 0 {
		e.pendingMu.Lock()
		p, ok := e.pending[req.RequestID]
		if ok {
			p.chunkCount += strings.Count(string(req.Body), "data:")
			p.streamText.Write(req.Body)
			// Each chunk is a bare SSE frame without a trailing blank line;
			// append the SSE frame separator so frames never concatenate.
			p.streamText.WriteString("\n\n")
			p.seenAt = time.Now()
		}
		e.pendingMu.Unlock()
		if !ok {
			e.debugLog("stream-chunk", fmt.Sprintf("reqId=%s NO PENDING ENTRY bodyLen=%d", req.RequestID, len(req.Body)))
		}
	}

	return e.okStreamResponse(nil, false)
}

// collectStreamSample finalizes a streaming sample when the request completes
// successfully. Only called from handleRequestComplete for succeeded streams.
func (e *Engine) collectStreamSample(reqID string, p *pendingInfo) {
	e.mu.RLock()
	probeEng := e.probeEngine
	cfg := e.cfg
	e.mu.RUnlock()

	if !cfg.IsProbeCacheEnabled() || probeEng == nil {
		return
	}

	chunks := [][]byte{[]byte(p.streamText.String())}
	text, ok := probeEng.ExtractStreamText(p.format, chunks)
	e.debugLog("stream-collect", fmt.Sprintf("reqId=%s hash=%s format=%s rawBytes=%d chunks=%d textOK=%v textLen=%d", reqID, p.hash[:min(len(p.hash), 12)], p.format, p.streamText.Len(), p.chunkCount, ok, len([]rune(text))))
	if !ok {
		raw := p.streamText.String()
		if len(raw) > 300 {
			raw = raw[:300]
		}
		e.debugLog("stream-collect", fmt.Sprintf("EMPTY_TEXT reqId=%s raw=%q", reqID, raw))
		return
	}
	// RawResponse 是 json.RawMessage：纯文本会破坏 MarshalIndent，导致整个
	// 状态文件保存静默失败（v0.3.0 流式样本从未落盘的根因）。合成最小合法
	// 协议对象（含 text），与回放合成共用同一实现。
	synthetic := probecache.SynthesizeRawResponse(p.format, p.model, text)
	probeEng.Store().AddSample(p.hash, p.model, p.format, text, synthetic)
	if err := probeEng.Store().LastError(); err != nil {
		e.debugLog("stream-collect", fmt.Sprintf("PERSIST_ERROR reqId=%s store=%s err=%v", reqID, "probe", err))
	}
	e.debugLog("stream-collect", fmt.Sprintf("sample hash=%s model=%s textLen=%d reqId=%s", p.hash, p.model, len([]rune(text)), reqID))
}

func (e *Engine) okEmptyResponse() ([]byte, error) {
	resBytes, _ := json.Marshal(types.RequestInterceptResponse{})
	return json.Marshal(types.Envelope{
		OK:     true,
		Result: resBytes,
	})
}

func (e *Engine) okStreamResponse(body []byte, drop bool) ([]byte, error) {
	resBytes, _ := json.Marshal(types.StreamChunkInterceptResponse{Body: body, DropChunk: drop})
	return json.Marshal(types.Envelope{
		OK:     true,
		Result: resBytes,
	})
}
