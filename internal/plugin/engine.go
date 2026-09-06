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
	reqBody  []byte
}

func NewEngine() *Engine {
	cfg := DefaultConfig()
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

	return &Engine{
		cfg:         cfg,
		store:       store,
		checker:     checker,
		probeEngine: probeEngine,
		pending:     make(map[string]pendingInfo),
	}
}

func (e *Engine) HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case types.MethodPluginRegister, types.MethodPluginReconfigure:
		return e.handleRegister(request)
	case types.MethodRequestInterceptBefore:
		return e.handleInterceptBefore(request)
	case types.MethodRequestInterceptAfter, types.MethodResponseInterceptAfter:
		e.debugLog("dispatch", "method="+method+" reqLen="+fmt.Sprintf("%d", len(request)))
		return e.handleInterceptAfter(request, method)
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
			RequestInterceptor:  true,
			ResponseInterceptor: true,
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

	// 1. 先进行 Guard 规则检查 (max_turns, thinking_signature, fuzzy circuit breaker, auto-fix)
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

	// 2. 若未被拦截，检查测活缓存命中 (probe cache)
	reqBody := req.GetRequestBody()
	if cfg.IsProbeCacheEnabled() && probeEng != nil && len(reqBody) > 0 {
		hash, eligible, isStream, format := probeEng.ComputeInputHash(reqBody)
		if eligible && hash != "" {
			if sample, hit := probeEng.Store().GetRandomSample(hash); hit {
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
			e.pendingMu.Lock()
			e.pending[req.RequestID] = pendingInfo{
				hash:     hash,
				model:    req.Model,
				format:   format,
				isStream: isStream,
				reqBody:  reqBody,
			}
			e.pendingMu.Unlock()
		}
	}

	return e.okEmptyResponse()
}

func (e *Engine) handleInterceptAfter(request []byte, method string) ([]byte, error) {
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

	if cfg.IsProbeCacheEnabled() && probeEng != nil && req.RequestID != "" {
		e.pendingMu.Lock()
		pi, found := e.pending[req.RequestID]
		delete(e.pending, req.RequestID)
		e.pendingMu.Unlock()

		if found && pi.hash != "" && pi.format == "chat_completions" {
			nowUnix := time.Now().Unix()
			textSnippet := "probe_response_cached"
			if len(pi.reqBody) > 0 {
				textSnippet = fmt.Sprintf("cached:%d:%s", len(pi.reqBody), pi.format)
			}
			syntheticResponse := map[string]any{
				"id":      fmt.Sprintf("chatcmpl-probe-%d", nowUnix),
				"object":  "chat.completion",
				"created": nowUnix,
				"model":   pi.model,
				"choices": []any{
					map[string]any{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": textSnippet,
						},
						"finish_reason":        "stop",
						"native_finish_reason": "stop",
					},
				},
				"usage": map[string]any{
					"prompt_tokens":     10,
					"completion_tokens": 5,
					"total_tokens":      15,
				},
			}
			rawResp, _ := json.Marshal(syntheticResponse)
			probeEng.Store().AddSample(pi.hash, pi.model, pi.format, textSnippet, rawResp)
		}
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
