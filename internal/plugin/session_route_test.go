package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cpa-codex-guard/internal/sessionroute"
	"cpa-codex-guard/internal/types"
	"gopkg.in/yaml.v3"
)

func configuredSessionEngine(t *testing.T, state string) *Engine {
	t.Helper()
	e := newTestEngine(t)
	config := fmt.Sprintf(`debug_log_enabled: false
state_file: %q
probe_state_file: %q
session_routing:
  enabled: true
  state_file: %q
  binding_ttl: 1h
  model_include_regex: '^gpt-'
  default_category: general
  categories:
    general: {preferred_auth_ids: [api]}
    benchmark: {preferred_auth_ids: [oauth]}
    probe: {max_priority: 6, exclude_auth_ids: [api, oauth]}
  rules:
    - category: benchmark
      models: [gpt-6-astra]
      text_all_regex: ['^Generate .* SVG']
    - category: probe
      models: [gpt-6.1-sol]
      user_agent_regex: '^Codex Desktop/'
      text_all_regex: ['1 到 355', '整数']
`, state+".guard", state+".probe", state)
	raw, _ := json.Marshal(types.LifecycleRequest{ConfigYAML: []byte(config)})
	result, err := e.HandleMethod(types.MethodPluginReconfigure, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env types.Envelope
	var registration types.Registration
	if json.Unmarshal(result, &env) != nil || !env.OK || json.Unmarshal(env.Result, &registration) != nil || !registration.Capabilities.Scheduler || !registration.Capabilities.SchedulerAcrossPriorities {
		t.Fatalf("routing scheduler was not registered: %s", result)
	}
	return e
}

func routeBefore(t *testing.T, e *Engine, req types.RequestInterceptRequest) types.RequestInterceptResponse {
	t.Helper()
	raw, _ := json.Marshal(req)
	data, err := e.HandleMethod(types.MethodRequestInterceptBefore, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env types.Envelope
	var response types.RequestInterceptResponse
	if json.Unmarshal(data, &env) != nil || !env.OK || json.Unmarshal(env.Result, &response) != nil {
		t.Fatalf("invalid before response: %s", data)
	}
	return response
}

func routePick(t *testing.T, e *Engine, marker string, metadata map[string]any, candidates ...sessionroute.Candidate) types.SchedulerPickResponse {
	t.Helper()
	req := types.SchedulerPickRequest{Model: "gpt-6-astra"}
	req.Options.Headers = http.Header{internalRouteHeader: []string{marker}}
	req.Options.Metadata = metadata
	for _, candidate := range candidates {
		req.Candidates = append(req.Candidates, struct {
			ID         string            `json:"ID"`
			Provider   string            `json:"Provider"`
			Priority   int               `json:"Priority"`
			Status     string            `json:"Status"`
			Attributes map[string]string `json:"Attributes"`
		}{candidate.ID, "codex", candidate.Priority, "active", nil})
	}
	raw, _ := json.Marshal(req)
	data, err := e.HandleMethod("scheduler.pick", raw)
	if err != nil {
		t.Fatal(err)
	}
	var env types.Envelope
	var response types.SchedulerPickResponse
	if json.Unmarshal(data, &env) != nil || !env.OK || json.Unmarshal(env.Result, &response) != nil {
		t.Fatalf("invalid scheduler response: %s", data)
	}
	return response
}

func TestSessionRoutingLifecycleAndFallback(t *testing.T) {
	state := t.TempDir() + "/bindings.json"
	e := configuredSessionEngine(t, state)
	metadata := map[string]any{"caller_scope": "caller-a"}
	headers := http.Header{"X-Session-Id": []string{"session-1"}, internalRouteHeader: []string{"client-spoof"}}
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "r1", Model: "gpt-6-astra", CallerScope: "caller-a", Metadata: metadata, Headers: headers, Body: []byte(`{"input":"Generate valid SVG"}`)})
	marker := before.Headers.Get(internalRouteHeader)
	if marker == "" || marker == "client-spoof" {
		t.Fatalf("internal header was not replaced: %+v", before)
	}
	if picked := routePick(t, e, marker, metadata, sessionroute.Candidate{ID: "api", Priority: 7}, sessionroute.Candidate{ID: "oauth", Priority: 7}); !picked.Handled || picked.AuthID != "oauth" {
		t.Fatalf("benchmark should select OAuth: %+v", picked)
	}
	// Reloaded instance must preserve the original classification and account.
	reloaded := configuredSessionEngine(t, state)
	second := routeBefore(t, reloaded, types.RequestInterceptRequest{RequestID: "r2", Model: "gpt-6-astra", CallerScope: "caller-a", Metadata: metadata, Headers: headers, Body: []byte(`{"input":"ordinary question"}`)})
	if picked := routePick(t, reloaded, second.Headers.Get(internalRouteHeader), metadata, sessionroute.Candidate{ID: "api", Priority: 7}, sessionroute.Candidate{ID: "oauth", Priority: 7}); !picked.Handled || picked.AuthID != "oauth" {
		t.Fatalf("existing session changed account: %+v", picked)
	}
	marker = second.Headers.Get(internalRouteHeader)
	if picked := routePick(t, reloaded, marker, metadata, sessionroute.Candidate{ID: "api", Priority: 7}); picked.Handled {
		t.Fatalf("unavailable account must defer to host: %+v", picked)
	}
	// An after-auth notification records the host's fallback and strips the header.
	afterReq, _ := json.Marshal(types.RequestInterceptRequest{RequestID: "r2", Headers: http.Header{internalRouteHeader: {"rewritten-marker"}}, Metadata: map[string]any{"selected_auth_id": "api"}})
	after, err := reloaded.HandleMethod(types.MethodRequestInterceptAfter, afterReq)
	if err != nil {
		t.Fatal(err)
	}
	var env types.Envelope
	var out types.RequestInterceptResponse
	_ = json.Unmarshal(after, &env)
	_ = json.Unmarshal(env.Result, &out)
	if len(out.ClearHeaders) != 1 || out.ClearHeaders[0] != internalRouteHeader {
		t.Fatalf("internal header was not cleared: %s", after)
	}
	if picked := routePick(t, reloaded, marker, metadata, sessionroute.Candidate{ID: "api", Priority: 7}); !picked.Handled || picked.AuthID != "api" {
		t.Fatalf("fallback binding not retained: %+v", picked)
	}
}

func TestSessionRoutingDoesNotTrustClientHeader(t *testing.T) {
	e := configuredSessionEngine(t, t.TempDir()+"/bindings.json")
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "r1", Model: "gpt-6-astra", Headers: http.Header{internalRouteHeader: []string{"forged"}}, Body: []byte(`{"input":"ordinary"}`)})
	if len(before.ClearHeaders) != 1 || before.Headers.Get(internalRouteHeader) != "" {
		t.Fatalf("forged internal marker not removed: %+v", before)
	}
	if picked := routePick(t, e, "forged", map[string]any{"caller_scope": "caller-a"}, sessionroute.Candidate{ID: "api"}); picked.Handled {
		t.Fatalf("forged marker was accepted: %+v", picked)
	}
}

func TestSessionRoutingCachePrecedesClassification(t *testing.T) {
	state := t.TempDir() + "/bindings.json"
	e := configuredSessionEngine(t, state)
	body := []byte(`{"model":"gpt-6-astra","input":"Generate a valid SVG"}`)
	hash, eligible, _, format := e.probeEngine.ComputeInputHash(body)
	if !eligible {
		t.Fatal("cache fixture is ineligible")
	}
	for i := 0; i < 3; i++ {
		e.probeEngine.Store().AddSample(hash, "gpt-6-astra", format, "cached", []byte(`{"id":"resp_cached","object":"response","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"id":"msg_cached","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"cached","annotations":[]}]}]}`))
	}
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "cached", Model: "gpt-6-astra", CallerScope: "caller-a", Body: body})
	if !before.Terminate || before.StatusCode != 200 {
		t.Fatalf("cache did not terminate request: %+v", before)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("cache hit touched classification/account state: %v", err)
	}
	e.pendingMu.Lock()
	_, pending := e.pending["cached"]
	e.pendingMu.Unlock()
	if pending {
		t.Fatal("cache hit created an account-selection record")
	}
	// An existing session stays alive without choosing an account on a hit.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.sessionRouter.store.Now = func() time.Time { return now }
	if _, err := e.sessionRouter.store.ClassifyOnce("caller-a", "header:session", "benchmark"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Minute)
	before = routeBefore(t, e, types.RequestInterceptRequest{RequestID: "cached-existing", Model: "gpt-6-astra", CallerScope: "caller-a", Headers: http.Header{"X-Session-Id": {"session"}}, Body: body})
	if !before.Terminate || before.Headers.Get(internalRouteHeader) != "" {
		t.Fatal("existing cached session entered routing")
	}
	now = now.Add(20 * time.Minute)
	key, _ := sessionroute.Key("caller-a", "header:session")
	binding, exists, err := e.sessionRouter.store.LookupKey(key)
	if err != nil || !exists || binding.Category != "benchmark" || binding.AuthID != "" {
		t.Fatalf("cache hit must refresh only existing TTL: %+v %v %v", binding, exists, err)
	}
}

func TestSessionRoutingCallerIsolationAndProbePool(t *testing.T) {
	e := configuredSessionEngine(t, t.TempDir()+"/bindings.json")
	headers := http.Header{"X-Session-Id": {"shared-session"}, "User-Agent": {"Codex Desktop/1"}}
	for _, tc := range []struct{ request, caller, model, body, want string }{
		{"a", "caller-a", "gpt-6-astra", `{"input":"Generate valid SVG"}`, "oauth"},
		{"b", "caller-b", "gpt-6-astra", `{"input":"ordinary"}`, "api"},
		{"c", "caller-c", "gpt-6.1-sol", `{"input":"选择 1 到 355 的整数"}`, "probe"},
	} {
		before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: tc.request, Model: tc.model, CallerScope: tc.caller, Headers: headers, Body: []byte(tc.body)})
		marker := before.Headers.Get(internalRouteHeader)
		picked := routePick(t, e, marker, map[string]any{"caller_scope": tc.caller}, sessionroute.Candidate{ID: "api", Priority: 7}, sessionroute.Candidate{ID: "oauth", Priority: 7}, sessionroute.Candidate{ID: "probe", Priority: 5})
		if !picked.Handled || picked.AuthID != tc.want {
			t.Fatalf("%s: got %+v, want %s", tc.request, picked, tc.want)
		}
		if picked := routePick(t, e, marker, map[string]any{"caller_scope": "different-caller"}, sessionroute.Candidate{ID: "api"}); picked.Handled {
			t.Fatal("another caller reused the route marker")
		}
	}
}

func TestSessionRoutingNoSessionDoesNotMergeRequests(t *testing.T) {
	e := configuredSessionEngine(t, t.TempDir()+"/bindings.json")
	for _, tc := range []struct{ id, body, want string }{
		{"r1", `{"input":"Generate valid SVG"}`, "oauth"},
		{"r2", `{"input":"ordinary"}`, "api"},
	} {
		before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: tc.id, Model: "gpt-6-astra", CallerScope: "caller", Body: []byte(tc.body)})
		picked := routePick(t, e, before.Headers.Get(internalRouteHeader), map[string]any{"caller_scope": "caller"}, sessionroute.Candidate{ID: "api"}, sessionroute.Candidate{ID: "oauth"})
		if picked.AuthID != tc.want {
			t.Fatalf("request-only routing: got %+v, want %s", picked, tc.want)
		}
	}
}

func TestSessionRoutingLocalCalculationSkipsState(t *testing.T) {
	state := t.TempDir() + "/bindings.json"
	e := configuredSessionEngine(t, state)
	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"max_output_tokens":50,"instructions":"You are a provider health-check endpoint. Answer the arithmetic challenge exactly and briefly.","input":"Calculate and respond with ONLY the number, nothing else.\n\nQ: 3 + 5 = ?\nA: 8\n\nQ: 12 - 7 = ?\nA: 5\n\nQ: 21 + 30 = ?\nA:"}`)
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "local", Model: "gpt-5.6-sol", CallerScope: "caller", Stream: true, Body: body})
	if !before.Terminate || !strings.Contains(string(before.ResponseBody), `"delta":"51"`) {
		t.Fatalf("local result did not terminate request: %+v", before)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("local calculation touched routing state: %v", err)
	}
}

func TestSessionRoutingIndependentOfCacheAndInvalidReload(t *testing.T) {
	e := configuredSessionEngine(t, t.TempDir()+"/bindings.json")
	config := *e.cfg
	config.ProbeCacheEnabled = boolPtr(false)
	configYAML, _ := yaml.Marshal(config)
	raw, _ := json.Marshal(types.LifecycleRequest{ConfigYAML: configYAML})
	data, _ := e.HandleMethod(types.MethodPluginReconfigure, raw)
	var env types.Envelope
	_ = json.Unmarshal(data, &env)
	if !env.OK {
		t.Fatalf("routing must work with cache disabled: %s", data)
	}
	config.SessionRouting.BindingTTL = "invalid"
	configYAML, _ = yaml.Marshal(config)
	raw, _ = json.Marshal(types.LifecycleRequest{ConfigYAML: configYAML})
	data, _ = e.HandleMethod(types.MethodPluginReconfigure, raw)
	_ = json.Unmarshal(data, &env)
	if env.OK {
		t.Fatal("invalid configuration was accepted")
	}
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "r", Model: "gpt-6-astra", CallerScope: "caller", Body: []byte(`{"input":"ordinary"}`)})
	picked := routePick(t, e, before.Headers.Get(internalRouteHeader), map[string]any{"caller_scope": "caller"}, sessionroute.Candidate{ID: "api"})
	if !picked.Handled || picked.AuthID != "api" {
		t.Fatalf("invalid reload replaced active router: %+v", picked)
	}
}

func TestSessionRoutingPreservesBodyRepair(t *testing.T) {
	e := configuredSessionEngine(t, t.TempDir()+"/bindings.json")
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "repaired", Model: "gpt-6-astra", CallerScope: "caller", Headers: http.Header{"X-OpenAI-Internal-Codex-Responses-Lite": {"true"}}, Body: []byte(`{"model":"gpt-6-astra","input":"ordinary"}`)})
	if !strings.Contains(string(before.Body), `"all_turns"`) || before.Headers.Get(internalRouteHeader) == "" {
		t.Fatalf("body repair or routing was lost: %+v", before)
	}
}

func TestDisabledSessionRoutingDoesNotEditHeaders(t *testing.T) {
	e := newTestEngine(t)
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "r", Model: "gpt-6-astra", CallerScope: "caller", Body: []byte(`{"input":"ordinary"}`)})
	if len(before.Headers) != 0 || len(before.ClearHeaders) != 0 {
		t.Fatalf("disabled routing edited headers: %+v", before)
	}
	raw, _ := e.HandleMethod(types.MethodRequestInterceptAfter, []byte(`{"RequestID":"r"}`))
	var env types.Envelope
	var after types.RequestInterceptResponse
	if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Result, &after) != nil || !env.OK {
		t.Fatalf("invalid after response: %s", raw)
	}
	if len(after.ClearHeaders) != 0 || len(after.Headers) != 0 {
		t.Fatalf("absent marker must not trigger host session reconciliation: %+v", after)
	}
}
