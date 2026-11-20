package sessionroute

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestEquivalentSessionRepresentations(t *testing.T) {
	id := "019a9b3c-1234-7123-8123-123456789abc"
	for _, tc := range []struct {
		headers http.Header
		body    string
		meta    map[string]any
	}{
		{headers: http.Header{"Session-Id": {id}}, body: `{}`},
		{headers: http.Header{"X-Session-Id": {id}}, body: `{}`},
		{body: `{"session_id":"` + id + `"}`},
		{body: `{}`, meta: map[string]any{"canonical_session_id": id}},
		{headers: http.Header{"Session-Id": {strings.ToUpper(id)}}, body: `{}`},
		{headers: http.Header{"X-Session-Id": {"different-client-id"}}, body: `{}`, meta: map[string]any{"canonical_session_id": id}},
	} {
		if got := SessionID(tc.headers, []byte(tc.body), tc.meta); got != id {
			t.Errorf("same session diverged: got %q, want %q", got, id)
		}
	}
	const opaque = "c28621bab78eacdb3ae128c0f6aaa0147842f063fda10ae9dc5473cc81d58985"
	for _, value := range []string{opaque, "lcp:v1:" + opaque, "derived:ctx:v1:" + opaque} {
		if got := canonicalSessionID(value); got != "2ad1939c-98ca-81da-8b69-3d084d5614c4" {
			t.Errorf("host golden mismatch: %q", got)
		}
	}
	for _, value := range []string{"session:", "\tunsafe", "unsafe\x01"} {
		if got := SessionID(http.Header{"Session-Id": {value}}, []byte(`{}`), nil); got != "" {
			t.Errorf("invalid session accepted: %q", got)
		}
	}
}

func TestNewSessionUsesFirstUserMessage(t *testing.T) {
	c := Config{Enabled: true, DefaultCategory: "general", BindingTTL: "1h", StateFile: "unused", ModelInclude: `^gpt-`, Categories: map[string]Route{"general": {PreferredAuthIDs: []string{"api"}}, "benchmark": {PreferredAuthIDs: []string{"oauth"}}}, Rules: []Rule{{Category: "benchmark", TextAll: []string{`^Generate .* SVG`}}}}
	if err := c.Compile(); err != nil {
		t.Fatal(err)
	}
	category, ok := c.Classify("gpt-6-astra", "client", []byte(`{"input":[{"role":"user","content":"Generate a valid SVG"},{"role":"assistant","content":"done"},{"role":"user","content":"ordinary"}]}`))
	if !ok || category != "benchmark" {
		t.Fatalf("first user message lost: %q %v", category, ok)
	}
}

func TestPriorityPoolRotatesAcrossStoreInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	max := 6
	routes := map[string]Route{"probe": {MaxPriority: &max}}
	counts := make(map[string]int)
	for i := 0; i < 6; i++ {
		s := &Store{Path: path, TTL: time.Hour}
		session := fmt.Sprint(i)
		if _, err := s.ClassifyOnce("caller", session, "probe"); err != nil {
			t.Fatal(err)
		}
		id, ok, err := s.Pick("caller", session, routes, []Candidate{{ID: "a", Priority: 5}, {ID: "b", Priority: 5}})
		if err != nil || !ok {
			t.Fatalf("pick failed: %v %v", ok, err)
		}
		counts[id]++
	}
	if counts["a"] != 3 || counts["b"] != 3 {
		t.Fatalf("pool did not rotate: %v", counts)
	}
}

func TestProbeContextConstraints(t *testing.T) {
	var c Config
	err := yaml.Unmarshal([]byte(`enabled: true
default_category: general
binding_ttl: 1h
state_file: unused
model_include_regex: '^gpt-'
categories:
  general: {preferred_auth_ids: [api]}
  probe: {max_priority: 6}
rules:
  - category: probe
    user_agent_regex: '^probe-client/'
    standalone: true
    max_input_bytes: 2048
    disallow_tools: true
    text_all_regex: ['^probe$']
`), &c)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Compile(); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"input":"probe","tools":[{"type":"function","name":"lookup"}]}`,
		`{"input":"probe","functions":[{"name":"lookup"}]}`,
		`{"input":"probe","tool_choice":"required"}`,
		`{"input":[{"role":"user","content":"probe"},{"role":"assistant","content":"done"},{"role":"user","content":"probe"}]}`,
		`{"input":"probe","previous_response_id":"resp_previous"}`,
		`{"input":"probe","conversation":{"id":"conv_existing"}}`,
		`{"input":[{"type":"function_call_output","output":"result"},{"role":"user","content":"probe"}]}`,
		`{"input":"probe","instructions":"` + strings.Repeat("x", 2048) + `"}`,
	} {
		if category, ok := c.Classify("gpt-6.1-sol", "probe-client/1", []byte(body)); !ok || category != "general" {
			t.Errorf("context bypassed safeguards: %s -> %q %v", body, category, ok)
		}
	}
	if category, ok := c.Classify("gpt-6.1-sol", "probe-client/1", []byte(`{"input":[{"role":"system","content":"health checker"},{"role":"user","content":"probe"}]}`)); !ok || category != "probe" {
		t.Fatalf("standalone probe was blocked: %q %v", category, ok)
	}
	c.Rules[0].UserAgent = ""
	c.Rules[0].CallerScopes = []string{"trusted-scope"}
	if err := c.Compile(); err != nil {
		t.Fatal(err)
	}
	for _, caller := range []string{"trusted-scope", "other"} {
		category, _ := c.ClassifyForCaller("gpt-6.1-sol", "any", caller, []byte(`{"input":"probe"}`))
		if (category == "probe") != (caller == "trusted-scope") {
			t.Fatalf("caller constraint ignored: %q -> %q", caller, category)
		}
	}
}

func TestAtomicResolveAndLegacyStateMigration(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "bindings.json")
	key, _ := Key("caller", "session")
	legacy, _ := json.Marshal(map[string]Binding{key: {Category: "benchmark", AuthID: "oauth", ExpiresAt: now.Add(time.Hour)}})
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	s := &Store{Path: path, TTL: time.Hour, Now: func() time.Time { return now }}
	called := 0
	classify := func() (string, bool) { called++; return "general", true }
	binding, ok, err := s.Resolve("caller", "session", classify)
	if err != nil || !ok || binding.AuthID != "oauth" || called != 0 {
		t.Fatalf("legacy live binding not preserved: %+v %v %v", binding, ok, err)
	}
	data, _ := os.ReadFile(path)
	state, err := decodeRoutingState(data)
	if err != nil || state.Version != 1 || state.Bindings[key].Category != "benchmark" {
		t.Fatalf("migration failed: %v", err)
	}
	now = now.Add(2 * time.Hour)
	binding, ok, err = s.Resolve("caller", "session", classify)
	if err != nil || !ok || binding.Category != "general" || binding.AuthID != "" || called != 1 {
		t.Fatalf("expired binding not reclassified: %+v %v %v", binding, ok, err)
	}
}

func TestConcurrentResolveClassifiesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	var called atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &Store{Path: path, TTL: time.Hour}
			_, ok, err := s.Resolve("caller", "session", func() (string, bool) { called.Add(1); return "general", true })
			if err != nil || !ok {
				t.Errorf("resolve failed: %v %v", ok, err)
			}
		}()
	}
	wg.Wait()
	if called.Load() != 1 {
		t.Fatalf("classified %d times", called.Load())
	}
}

func TestWeightedPoolStickyBindingsAndZeroWeight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	max := 6
	routes := map[string]Route{"probe": {MaxPriority: &max}}
	a, b, zero := int64(3), int64(1), int64(0)
	counts := map[string]int{}
	for i := 0; i < 16; i++ {
		s := &Store{Path: path, TTL: time.Hour}
		session := fmt.Sprint(i)
		_, _ = s.ClassifyOnce("caller", session, "probe")
		candidates := []Candidate{{ID: "a", Priority: 5, Weight: &a}, {ID: "b", Priority: 5, Weight: &b}, {ID: "zero", Priority: 6, Weight: &zero}, {ID: "lower", Priority: 4}}
		id, ok, err := s.Pick("caller", session, routes, candidates)
		if err != nil || !ok {
			t.Fatalf("pick: %v %v", ok, err)
		}
		counts[id]++
		for retry := 0; retry < 3; retry++ {
			sticky, _, err := s.Pick("caller", session, routes, candidates)
			if err != nil || sticky != id {
				t.Fatalf("sticky pick changed: %q -> %q %v", id, sticky, err)
			}
		}
	}
	if counts["a"] != 12 || counts["b"] != 4 {
		t.Fatalf("weight ratio/rotation changed: %v", counts)
	}
}

func TestPoolCreditsSurviveTemporarySubset(t *testing.T) {
	max := 6
	route := Route{MaxPriority: &max}
	pools := map[string]poolState{}
	expires := time.Now().Add(time.Hour)
	all := []Candidate{{ID: "a", Priority: 5}, {ID: "b", Priority: 5}}
	for i, candidates := range [][]Candidate{all, all[:1], all} {
		id, err := choose(route, candidates, "probe", pools, expires)
		want := []string{"a", "a", "b"}[i]
		if err != nil || id != want {
			t.Fatalf("step %d lost credits: %q want %q (%v)", i, id, want, err)
		}
	}
	weight := int64(3)
	all[0].Weight = &weight
	id, err := choose(route, all, "probe", pools, expires)
	if err != nil || id != "a" {
		t.Fatalf("weight change was not applied: %q %v", id, err)
	}
}

func TestUnsafePoolRulesRejectConfiguration(t *testing.T) {
	max := 6
	base := Config{Enabled: true, DefaultCategory: "general", BindingTTL: "1h", StateFile: "unused", ModelInclude: `^gpt-`, Categories: map[string]Route{"general": {PreferredAuthIDs: []string{"api"}}, "probe": {MaxPriority: &max}}}
	for _, rule := range []Rule{
		{Category: "probe", TextAll: []string{"probe"}},
		{Category: "probe", Standalone: true, TextAll: []string{"probe"}, MaxInputBytes: 2048},
		{Category: "probe", Standalone: true, TextAll: []string{"probe"}, UserAgent: `^probe/`},
	} {
		c := base
		c.Rules = []Rule{rule}
		if err := c.Compile(); err == nil {
			t.Fatalf("unsafe pool rule accepted: %+v", rule)
		}
	}
	base.DefaultCategory = "probe"
	if err := base.Compile(); err == nil {
		t.Fatal("pool default permits uncertain context to become a probe")
	}
}
