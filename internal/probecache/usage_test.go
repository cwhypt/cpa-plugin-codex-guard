package probecache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealStreamUsageAndLegacyCleanup(t *testing.T) {
	stream := "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_real","object":"response","status":"completed","output":[{"id":"msg_real","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"new","annotations":[]}]}],"usage":{"input_tokens":316,"output_tokens":5,"total_tokens":321}}}` + "\n\n"
	real := StreamResponse("responses", stream)
	if real == nil {
		t.Fatal("real usage in completed response was not captured")
	}
	if StreamResponse("responses", strings.ReplaceAll(stream, "response.completed", "response.failed")) != nil {
		t.Fatal("failed event must not be cached")
	}
	old := json.RawMessage(`{"id":"chatcmpl-cached-123","usage":{"input_tokens":0,"output_tokens":4,"total_tokens":4}}`)
	path := filepath.Join(t.TempDir(), "cache.json")
	state := StateData{Entries: map[string]ProbeEntry{
		"mixed": {InputHash: "mixed", Format: "responses", ExpireAt: time.Now().Add(time.Hour), Activated: true,
			Samples: []Sample{{Text: "old", RawResponse: old}, {Text: "new", RawResponse: real}}},
		"bad": {InputHash: "bad", Format: "responses", ExpireAt: time.Now().Add(time.Hour), Activated: true,
			Samples: []Sample{{Text: "old", RawResponse: old}}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path, time.Hour, 1)
	if store.LastError() != nil {
		t.Fatal(store.LastError())
	}
	if _, ok := store.PeekSamples("bad"); ok {
		t.Fatal("legacy synthetic-only entry must be removed")
	}
	samples, ok := store.PeekSamples("mixed")
	if !ok || len(samples) != 1 || samples[0].Text != "new" {
		t.Fatalf("real sample not preserved: %+v", samples)
	}
	reloaded := NewStore(path, time.Hour, 1)
	sample, ok := reloaded.GetRandomSample("mixed")
	if !ok {
		t.Fatal("real sample did not survive reload")
	}
	usage, valid := realUsage(sample.RawResponse, "responses")
	if !valid || usage["input_tokens"] != float64(316) {
		t.Fatalf("real sample did not survive reload: %+v", sample)
	}
	if _, ok := reloaded.PeekSamples("bad"); ok {
		t.Fatal("cleanup must be persisted")
	}
}
