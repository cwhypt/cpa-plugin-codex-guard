package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"cpa-codex-guard/internal/guard"
	"cpa-codex-guard/internal/probecache"
	"cpa-codex-guard/internal/state"
	"cpa-codex-guard/internal/types"
)

// debugLog is temporary diagnostic instrumentation (kept per user request).
func (e *Engine) debugLog(stage, msg string) {
	f, err := os.OpenFile("/home/cwhypt/cliproxyapi/data/cpa-codex-guard-debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
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

	pendingMu sync.RWMutex
	pending   map[string]pendingInfo
}

type pendingInfo struct {
	hash     string
	model    string
	format   string
	isStream bool
	seenAt   time.Time
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
		pending:     make(map[string]pendingInfo),
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
				e.pendingMu.Lock()
				e.pending[req.RequestID] = pendingInfo{
					hash:     hash,
					model:    req.Model,
					format:   format,
					isStream: isStream,
					seenAt:   time.Now(),
				}
				e.pendingMu.Unlock()
			}
		}
	}

	return e.okEmptyResponse()
}

// handleRequestInterceptAfter fires after credential selection, before upstream
// execution. Its Body is the rewritten upstream payload — NOT a response. No
// sample collection happens here.
func (e *Engine) handleRequestInterceptAfter(request []byte) ([]byte, error) {
	var req types.RequestInterceptRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return e.okEmptyResponse()
		}
	}

	e.mu.RLock()
	checker := e.checker
	e.mu.RUnlock()

	checker.ObserveResponse(&req)
	return e.okEmptyResponse()
}

// handleResponseInterceptAfter fires only for successful non-streaming
// upstream responses (StatusCode=200). Body is the upstream response body and
// RequestBody is the exact payload sent upstream — this is the authoritative
// sample collection point.
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
		reqBody := req.GetRequestBody()
		respBody := req.GetResponseBody()
		if len(reqBody) > 0 && len(respBody) > 0 {
			hash, eligible, _, format := probeEng.ComputeInputHash(reqBody)
			if eligible && hash != "" {
				if text, ok := probeEng.ExtractOutputText(format, respBody); ok {
					probeEng.Store().AddSample(hash, req.Model, format, text, respBody)
					e.debugLog("collect", fmt.Sprintf("sample hash=%s model=%s textLen=%d reqId=%s", hash, req.Model, len([]rune(text)), req.RequestID))
				}
			}
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
// success or failure. It exists to evict pending entries for failed requests
// so they never leak; success cleanup normally happens in the response hook.
func (e *Engine) handleRequestComplete(request []byte) ([]byte, error) {
	var completion types.RequestCompletion
	if len(request) > 0 {
		if err := json.Unmarshal(request, &completion); err != nil {
			return e.okEmptyResponse()
		}
	}

	e.debugLog("complete", fmt.Sprintf("reqId=%s outcome=%s status=%d err=%s", completion.RequestID, completion.Outcome, completion.StatusCode, completion.Error))

	if completion.RequestID != "" {
		e.pendingMu.Lock()
		delete(e.pending, completion.RequestID)
		e.pendingMu.Unlock()
	}

	return e.okEmptyResponse()
}

func (e *Engine) okEmptyResponse() ([]byte, error) {
	resBytes, _ := json.Marshal(types.RequestInterceptResponse{})
	return json.Marshal(types.Envelope{
		OK:     true,
		Result: resBytes,
	})
}
