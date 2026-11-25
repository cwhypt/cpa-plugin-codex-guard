package plugin

import (
	"encoding/json"
	"testing"

	"cpa-codex-guard/internal/types"
)

func TestClaudeHealthProbe(t *testing.T) {
	engine := newTestEngine(t)
	for _, tc := range []struct {
		format string
		stream bool
		body   string
		want   bool
	}{
		{"claude", false, `{"model":"claude-haiku-5-5","max_tokens":1,"messages":[{"role":"user","content":"."}]}`, true},
		{"claude", true, `{"model":"claude-haiku-5-5","max_tokens":1,"messages":[{"role":"user","content":"."}]}`, false},
		{"openai", false, `{"model":"claude-haiku-5-5","max_tokens":1,"messages":[{"role":"user","content":"."}]}`, false},
		{"claude", false, `{"model":"claude-haiku-5-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, false},
		{"claude", false, `{"model":"claude-haiku-5-5","max_tokens":1,"messages":[{"role":"user","content":"."}],"system":"test"}`, false},
	} {
		raw, _ := json.Marshal(types.RequestInterceptRequest{SourceFormat: tc.format, Stream: tc.stream, Model: "claude-haiku-5-5", Body: []byte(tc.body)})
		result, err := engine.HandleMethod(types.MethodRequestInterceptBefore, raw)
		if err != nil {
			t.Fatal(err)
		}
		var envelope types.Envelope
		var response types.RequestInterceptResponse
		if json.Unmarshal(result, &envelope) != nil || json.Unmarshal(envelope.Result, &response) != nil {
			t.Fatal("invalid envelope")
		}
		if response.Terminate != tc.want {
			t.Fatalf("format=%s stream=%v body=%s: terminate=%v", tc.format, tc.stream, tc.body, response.Terminate)
		}
		if tc.want {
			var message struct {
				Type  string         `json:"type"`
				Model string         `json:"model"`
				Usage map[string]int `json:"usage"`
			}
			if json.Unmarshal(response.ResponseBody, &message) != nil || response.StatusCode != 200 || message.Type != "message" || message.Model != "claude-haiku-5-5" || message.Usage["input_tokens"] != 0 {
				t.Fatalf("invalid Claude response: %s", response.ResponseBody)
			}
		}
	}
}
