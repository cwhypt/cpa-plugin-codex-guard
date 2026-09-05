package types

import (
	"encoding/json"
	"testing"
)

func TestRequestInterceptResponseSerialization(t *testing.T) {
	resp := RequestInterceptResponse{
		Terminate:    true,
		StatusCode:   400,
		ResponseBody: []byte(`{"error":"bad request"}`),
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal map failed: %v", err)
	}

	if m["Terminate"] != true {
		t.Errorf("expected Terminate true, got %v", m["Terminate"])
	}
	if int(m["StatusCode"].(float64)) != 400 {
		t.Errorf("expected StatusCode 400, got %v", m["StatusCode"])
	}
}
