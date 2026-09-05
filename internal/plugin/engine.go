package plugin

import (
	"encoding/json"
	"net/http"
	"sync"

	"cpa-codex-guard/internal/guard"
	"cpa-codex-guard/internal/state"
	"cpa-codex-guard/internal/types"
)

type Engine struct {
	mu      sync.RWMutex
	cfg     *Config
	store   *state.Store
	checker *guard.Checker
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

	return &Engine{
		cfg:     cfg,
		store:   store,
		checker: checker,
	}
}

func (e *Engine) HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case types.MethodPluginRegister, types.MethodPluginReconfigure:
		return e.handleRegister(request)
	case types.MethodRequestInterceptBefore:
		return e.handleInterceptBefore(request)
	case types.MethodRequestInterceptAfter, types.MethodResponseInterceptAfter:
		return e.handleInterceptAfter(request)
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
			Description:      "Guards against max_turns, invalid signatures, auto-fixes Responses-Lite context, and fuzzy circuit breaks repeated errors",
			GitHubRepository: "https://github.com/cwhypt/cliproxyapi",
			ConfigFields: []types.ConfigField{
				{Name: "state_file", Type: "string", Description: "Path to state persistence file"},
				{Name: "ttl", Type: "string", Description: "TTL for invalid signature cache"},
				{Name: "circuit_ttl", Type: "string", Description: "TTL for fuzzy circuit breaker (default 3h)"},
				{Name: "block_max_turns", Type: "boolean", Description: "Block requests with max_turns parameter"},
				{Name: "block_invalid_signatures", Type: "boolean", Description: "Block requests with known invalid thinking signatures"},
				{Name: "autofix_responses_lite", Type: "boolean", Description: "Auto-fix missing reasoning.context for Responses-Lite requests"},
				{Name: "fuzzy_circuit_breaker", Type: "boolean", Description: "Enable session-level fuzzy similarity circuit breaker"},
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
	e.mu.RUnlock()

	res := checker.CheckRequest(&req)
	if res == nil {
		return e.okEmptyResponse()
	}

	resBytes, err := json.Marshal(res)
	if err != nil {
		return e.okEmptyResponse()
	}

	return json.Marshal(types.Envelope{
		OK:     true,
		Result: resBytes,
	})
}

func (e *Engine) handleInterceptAfter(request []byte) ([]byte, error) {
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

func (e *Engine) okEmptyResponse() ([]byte, error) {
	resBytes, _ := json.Marshal(types.RequestInterceptResponse{})
	return json.Marshal(types.Envelope{
		OK:     true,
		Result: resBytes,
	})
}
