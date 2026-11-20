package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cpa-codex-guard/internal/sessionroute"
	"cpa-codex-guard/internal/types"
)

func TestRoutingStreamAccumulationBoundary(t *testing.T) {
	for _, size := range []int{10, 21000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			e := configuredSessionEngine(t, t.TempDir()+"/bindings.json")
			body, _ := json.Marshal(map[string]any{"input": strings.Repeat("x", size), "stream": true})
			routeBefore(t, e, types.RequestInterceptRequest{RequestID: "r", Model: "gpt-6-astra", CallerScope: "caller", Stream: true, Body: body})
			for _, chunk := range []string{strings.Repeat("x", maxStreamAccumBytes-3), "x", "another chunk"} {
				raw, _ := json.Marshal(types.StreamChunkInterceptRequest{RequestID: "r", ChunkIndex: 1, Body: []byte(chunk)})
				result, err := e.HandleMethod(types.MethodResponseInterceptStreamChunk, raw)
				var env types.Envelope
				if err != nil || json.Unmarshal(result, &env) != nil || !env.OK {
					t.Fatalf("stream boundary failed: %s %v", result, err)
				}
			}
			if !e.pendingMu.TryLock() {
				t.Fatal("stream handler leaked pendingMu")
			}
			defer e.pendingMu.Unlock()
			p := e.pending["r"]
			if size > e.cfg.ProbeMaxInputBytes && (p.streamText.Len() != 0 || p.chunkCount != 0) {
				t.Fatal("routing-only stream was collected as a cache sample")
			}
			if p.streamText.Len() > maxStreamAccumBytes {
				t.Fatal("stream accumulation exceeded cap")
			}
		})
	}
}

func TestExpiredSessionIsReclassifiedAtomically(t *testing.T) {
	e := configuredSessionEngine(t, t.TempDir()+"/bindings.json")
	initial := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.sessionRouter.store.Now = func() time.Time { return initial }
	headers := http.Header{"X-Session-Id": {"session"}}
	session := sessionroute.SessionID(headers, []byte(`{}`), nil)
	if _, err := e.sessionRouter.store.ClassifyOnce("caller", session, "benchmark"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	e.sessionRouter.store.Now = func() time.Time {
		calls++
		if calls == 1 {
			return initial.Add(time.Hour - time.Nanosecond)
		}
		return initial.Add(time.Hour + time.Nanosecond)
	}
	routeBefore(t, e, types.RequestInterceptRequest{RequestID: "r", Model: "gpt-6-astra", CallerScope: "caller", Headers: headers, Body: []byte(`{"input":"ordinary"}`)})
	key, _ := sessionroute.Key("caller", session)
	binding, exists, err := e.sessionRouter.store.LookupKey(key)
	// An atomic transaction either renews the still-valid old category, or
	// classifies an expired one. It must not recreate the old category after expiry.
	if err != nil || !exists {
		t.Fatalf("missing binding: %+v %v", binding, err)
	}
	if binding.Category == "benchmark" && binding.ExpiresAt.After(initial.Add(2*time.Hour)) {
		t.Fatalf("expired category was resurrected: %+v", binding)
	}
}

func TestSchedulerWeightABIAndRotation(t *testing.T) {
	for _, tc := range []struct {
		candidate types.SchedulerCandidate
		want      int64
	}{
		{types.SchedulerCandidate{}, 1},
		{types.SchedulerCandidate{Metadata: map[string]any{"weight": ""}}, 1},
		{types.SchedulerCandidate{Attributes: map[string]string{"weight": "3"}}, 3},
		{types.SchedulerCandidate{Attributes: map[string]string{"weight": "2"}, Metadata: map[string]any{"weight": float64(9)}}, 2},
		{types.SchedulerCandidate{Metadata: map[string]any{"weight": float64(4)}}, 4},
		{types.SchedulerCandidate{Attributes: map[string]string{"weight": "0"}}, 0},
		{types.SchedulerCandidate{Attributes: map[string]string{"weight": "1.5"}}, 0},
		{types.SchedulerCandidate{Attributes: map[string]string{"weight": "1000001"}}, 0},
	} {
		if got := schedulerWeight(tc.candidate); got != tc.want {
			t.Errorf("weight got %d want %d", got, tc.want)
		}
	}
	e := configuredSessionEngine(t, t.TempDir()+"/bindings.json")
	a, b := int64(3), int64(1)
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		id := fmt.Sprint(i)
		before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: id, Model: "gpt-6.1-sol", CallerScope: "caller", Headers: http.Header{"X-Session-Id": {id}, "User-Agent": {"Codex Desktop/1"}}, Body: []byte(`{"input":"选择 1 到 355 的整数"}`)})
		picked := routePick(t, e, before.Headers.Get(internalRouteHeader), map[string]any{"caller_scope": "caller"}, sessionroute.Candidate{ID: "a", Priority: 5, Weight: &a}, sessionroute.Candidate{ID: "b", Priority: 5, Weight: &b})
		if !picked.Handled {
			t.Fatal("scheduler did not handle probe")
		}
		counts[picked.AuthID]++
	}
	if counts["a"] != 6 || counts["b"] != 2 {
		t.Fatalf("ABI weights ignored: %v", counts)
	}
}

func TestCanonicalSessionSurvivesTransportRepresentationChange(t *testing.T) {
	path := t.TempDir() + "/bindings.json"
	e := configuredSessionEngine(t, path)
	id := "019a9b3c-1234-7123-8123-123456789abc"
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "first", Model: "gpt-6-astra", CallerScope: "caller", Headers: http.Header{"Session-Id": {strings.ToUpper(id)}}, Body: []byte(`{"input":"Generate valid SVG"}`)})
	metadata := map[string]any{"caller_scope": "caller"}
	candidates := []sessionroute.Candidate{{ID: "api", Priority: 7}, {ID: "oauth", Priority: 7}}
	if picked := routePick(t, e, before.Headers.Get(internalRouteHeader), metadata, candidates...); picked.AuthID != "oauth" {
		t.Fatalf("first pick: %+v", picked)
	}
	e = configuredSessionEngine(t, path)
	before = routeBefore(t, e, types.RequestInterceptRequest{RequestID: "second", Model: "gpt-6-astra", CallerScope: "caller", Headers: http.Header{"X-Session-Id": {id}}, Body: []byte(`{"input":"ordinary"}`)})
	if picked := routePick(t, e, before.Headers.Get(internalRouteHeader), metadata, candidates...); picked.AuthID != "oauth" {
		t.Fatalf("header change lost sticky binding: %+v", picked)
	}
	before = routeBefore(t, e, types.RequestInterceptRequest{RequestID: "third", Model: "gpt-6-astra", CallerScope: "caller", Metadata: map[string]any{"canonical_session_id": id}, Body: []byte(`{"input":"ordinary"}`)})
	if picked := routePick(t, e, before.Headers.Get(internalRouteHeader), metadata, candidates...); picked.AuthID != "oauth" {
		t.Fatalf("canonical metadata lost sticky binding: %+v", picked)
	}
}
