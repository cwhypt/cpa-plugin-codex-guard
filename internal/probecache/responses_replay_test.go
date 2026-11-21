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

func TestResponsesReasoningReplayAndEncryptedFallback(t *testing.T) {
	store := NewStore("", time.Hour, 1)
	e := NewEngine(store, 20480, 200)
	raw := `{"id":"resp_old","object":"response","status":"completed","output":[{"id":"rs_old","type":"reasoning","summary":[{"type":"summary_text","text":"Check the answer."}]},{"id":"msg_old","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"21","annotations":[]}]}],"usage":{"input_tokens":245,"output_tokens":1933,"total_tokens":2178,"attribution":{"items":{"msg_input":{"input_tokens":245},"rs_old":{"output_tokens":1931},"msg_old":{"output_tokens":2}}}}}`
	sample := &Sample{Text: "21", RawResponse: []byte(raw)}
	if !store.AddSample("hash", "m", "responses", sample.Text, sample.RawResponse) {
		t.Fatal("unencrypted reasoning should activate cache")
	}
	if _, hit := store.GetRandomSample("hash"); !hit {
		t.Fatal("unencrypted reasoning sample not available")
	}
	if nonstream := e.FormatResponse(sample, false, "responses", "m"); nonstream == nil || !json.Valid(nonstream.ResponseBody) {
		t.Fatal("unencrypted reasoning non-stream replay must be valid JSON")
	}
	response := e.FormatResponse(sample, true, "responses", "m")
	if response == nil {
		t.Fatal("expected replayable unencrypted reasoning")
	}
	var result map[string]any
	var reasonID string
	for _, line := range strings.Split(string(response.ResponseBody), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "response.reasoning_summary_text.delta" {
			reasonID = event["item_id"].(string)
		}
		if event["type"] == "response.completed" {
			result = event["response"].(map[string]any)
		}
	}
	items := result["output"].([]any)
	if reasonID == "" || items[0].(map[string]any)["id"] != reasonID {
		t.Fatal("reasoning event ID does not match completed output")
	}
	usage := result["usage"].(map[string]any)
	attr := usage["attribution"].(map[string]any)["items"].(map[string]any)
	if attr[reasonID] == nil || attr[items[1].(map[string]any)["id"].(string)] == nil || usage["output_tokens"] != float64(1933) {
		t.Fatalf("reasoning attribution or usage incorrect: %+v", usage)
	}
	if strings.Contains(string(response.ResponseBody), "rs_old") || strings.Contains(string(response.ResponseBody), "msg_old") {
		t.Fatal("historical output IDs leaked")
	}
	encrypted := strings.Replace(raw, `"id":"rs_old"`, `"id":"rs_old","encrypted_content":"opaque"`, 1)
	if e.FormatResponse(&Sample{Text: "21", RawResponse: []byte(encrypted)}, true, "responses", "m") != nil {
		t.Fatal("encrypted reasoning must bypass cache")
	}
	if store.AddSample("encrypted", "m", "responses", "21", []byte(encrypted)) {
		t.Fatal("encrypted reasoning must not activate cache")
	}
}

func TestResponsesReasoningReplayBoundaries(t *testing.T) {
	e := NewEngine(NewStore("", time.Hour, 1), 20480, 200)
	raw := `{"id":"resp_original","object":"response","status":"completed","output":[{"id":"rs_original","type":"reasoning","summary":[{"type":"summary_text","text":"Check."},{"type":"summary_text","text":"Confirm."}]},{"id":"rs_second","type":"reasoning","summary":[]},{"id":"msg_original","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"21","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":10,"total_tokens":15}}`
	response := e.FormatResponse(&Sample{Text: "21", RawResponse: []byte(raw)}, true, "responses", "m")
	if response == nil {
		t.Fatal("multiple reasoning items and summaries should be replayable")
	}
	var outputs []any
	var events []map[string]any
	for _, line := range strings.Split(string(response.ResponseBody), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["sequence_number"] != float64(len(events)) {
			t.Fatal("non-contiguous sequence number")
		}
		events = append(events, event)
		if event["type"] == "response.completed" {
			outputs = event["response"].(map[string]any)["output"].([]any)
		}
	}
	if len(outputs) != 3 {
		t.Fatalf("expected two reasoning items and one message, got %d", len(outputs))
	}
	var summaryParts, doneItems int
	for _, event := range events {
		index, indexed := event["output_index"].(float64)
		if !indexed {
			continue
		}
		if index < 0 || index >= float64(len(outputs)) {
			t.Fatalf("invalid output index: %v", index)
		}
		item := outputs[int(index)].(map[string]any)
		if id, ok := event["item_id"].(string); ok && id != item["id"] {
			t.Fatal("event references the wrong output item")
		}
		if added, ok := event["item"].(map[string]any); ok && added["id"] != item["id"] {
			t.Fatal("added/done item ID does not match completed output")
		}
		if event["type"] == "response.reasoning_summary_text.done" {
			parts := item["summary"].([]any)
			summaryIndex := event["summary_index"].(float64)
			if summaryIndex != float64(summaryParts) || event["text"] != parts[int(summaryIndex)].(map[string]any)["text"] {
				t.Fatal("summary index/text does not match completed output")
			}
			summaryParts++
		}
		if event["type"] == "response.output_item.done" {
			doneItems++
		}
	}
	if summaryParts != 2 || doneItems != 3 {
		t.Fatal("missing summary or output completion events")
	}
	if text, ok := e.ExtractStreamText("responses", [][]byte{response.ResponseBody}); !ok || text != "21" {
		t.Fatal("summary text must not become the cached answer")
	}
	for _, invalid := range []struct{ name, from, to string }{
		{"duplicate ID", `"id":"rs_second"`, `"id":"rs_original"`},
		{"tool output", `"type":"reasoning"`, `"type":"function_call"`},
		{"encrypted", `"summary":[]`, `"summary":[],"encrypted_content":"opaque"`},
		{"unfinished", `"summary":[]`, `"summary":[],"status":"in_progress"`},
		{"hidden content", `"summary":[]`, `"summary":[],"content":[{"type":"reasoning_text","text":"hidden"}]`},
		{"unknown field", `"summary":[]`, `"summary":[],"unknown":"value"`},
		{"null summary", `"summary":[]`, `"summary":null`},
		{"non-text summary", `"text":"Check."`, `"text":7`},
		{"unknown summary field", `"text":"Check."`, `"text":"Check.","unknown":"value"`},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			body := []byte(strings.Replace(raw, invalid.from, invalid.to, 1))
			for _, stream := range []bool{false, true} {
				if e.FormatResponse(&Sample{Text: "21", RawResponse: body}, stream, "responses", "m") != nil {
					t.Fatalf("unsafe sample replayed (stream=%v)", stream)
				}
			}
			if e.Store().AddSample("invalid", "m", "responses", "21", body) {
				t.Fatal("unsafe sample activated the cache")
			}
		})
	}
}
