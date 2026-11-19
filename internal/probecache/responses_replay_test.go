package probecache

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResponsesReplayIDsAndEvents(t *testing.T) {
	e := NewEngine(NewStore("", time.Hour, 1), 20480, 200)
	sample := &Sample{Text: "Hi!", RawResponse: []byte(`{"id":"resp_old","object":"response","status":"completed","model":"m","output":[{"id":"msg_old_output","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hi!","annotations":[]}]}],"usage":{"input_tokens":304,"output_tokens":6,"total_tokens":310,"attribution":{"items":{"msg_old_input":{"input_tokens":6,"output_tokens":0},"msg_old_output":{"input_tokens":2,"output_tokens":6}}}}}`)}
	var ids []string
	for _, stream := range []bool{false, true, true} {
		resp := e.FormatResponse(sample, stream, "responses", "m")
		if resp == nil {
			t.Fatal("expected valid replay")
		}
		body := string(resp.ResponseBody)
		if strings.Contains(body, "msg_old_") || strings.Contains(body, "resp_old") {
			t.Fatalf("old IDs leaked: %s", body)
		}
		var result map[string]any
		if stream {
			var eventNames []string
			var deltaID string
			var referencedItems, referencedResponses []string
			for _, line := range strings.Split(body, "\n") {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				kind := event["type"].(string)
				eventNames = append(eventNames, kind)
				if event["sequence_number"] != float64(len(eventNames)-1) {
					t.Fatalf("invalid sequence number: %+v", event)
				}
				if id, ok := event["item_id"].(string); ok {
					referencedItems = append(referencedItems, id)
				}
				if item, ok := event["item"].(map[string]any); ok {
					referencedItems = append(referencedItems, item["id"].(string))
				}
				if response, ok := event["response"].(map[string]any); ok {
					referencedResponses = append(referencedResponses, response["id"].(string))
				}
				if kind == "response.output_text.delta" {
					deltaID = event["item_id"].(string)
				}
				if kind == "response.completed" {
					result = event["response"].(map[string]any)
				}
			}
			if len(eventNames) != 9 || eventNames[1] != "response.in_progress" || eventNames[7] != "response.output_item.done" {
				t.Fatalf("unexpected events: %v", eventNames)
			}
			item := result["output"].([]any)[0].(map[string]any)
			if item["id"] != deltaID {
				t.Fatal("delta and output IDs differ")
			}
			for _, id := range referencedItems {
				if id != item["id"] {
					t.Fatal("event item IDs differ")
				}
			}
			for _, id := range referencedResponses {
				if id != result["id"] {
					t.Fatal("event response IDs differ")
				}
			}
		} else if err := json.Unmarshal(resp.ResponseBody, &result); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, result["id"].(string))
		itemID := result["output"].([]any)[0].(map[string]any)["id"].(string)
		usage := result["usage"].(map[string]any)
		attrib := usage["attribution"].(map[string]any)["items"].(map[string]any)
		if len(attrib) != 2 || attrib[itemID] == nil || usage["input_tokens"] != float64(304) {
			t.Fatalf("invalid attribution: %+v", usage)
		}
	}
	if ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Fatalf("response IDs reused: %v", ids)
	}
	if e.FormatResponse(&Sample{Text: "Hi!", RawResponse: []byte(`{"id":"old","usage":{"input_tokens":3,"output_tokens":1}}`)}, true, "responses", "m") != nil {
		t.Fatal("incomplete sample must bypass replay")
	}
}

func TestIncompatibleSamplesDoNotPreventRecollection(t *testing.T) {
	store := NewStore("", time.Hour, 2)
	old := json.RawMessage(`{"id":"old","usage":{"input_tokens":3,"output_tokens":1}}`)
	store.entries["hash"] = ProbeEntry{InputHash: "hash", Model: "m", Format: "responses", Activated: true, ExpireAt: time.Now().Add(time.Hour), Samples: []Sample{{Text: "Hi!", RawResponse: old}, {Text: "Hi!", RawResponse: old}}}
	if _, hit := store.GetRandomSample("hash"); hit {
		t.Fatal("incompatible historical samples must not be replayed")
	}
	real := []byte(`{"id":"resp_real","object":"response","status":"completed","output":[{"id":"msg_real","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hi!","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":1}}`)
	store.AddSample("hash", "m", "responses", "Hi!", real)
	if _, hit := store.GetRandomSample("hash"); hit {
		t.Fatal("one usable sample must not activate a two-sample pool")
	}
	store.AddSample("hash", "m", "responses", "Hi!", real)
	sample, hit := store.GetRandomSample("hash")
	if !hit || string(sample.RawResponse) != string(real) || len(store.entries["hash"].Samples) != 4 {
		t.Fatal("expected usable replay and preserved historical samples")
	}
}
