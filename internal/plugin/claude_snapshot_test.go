package plugin

import (
	"encoding/json"
	"testing"
)

func TestParseClaudeThinkingSSERequiresCompleteMessage(t *testing.T) {
	raw := []byte("data: {\"type\":\"message_start\"}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n")
	if blocks, complete := parseClaudeThinkingSSE(raw); complete || len(blocks) != 0 {
		t.Fatalf("expected incomplete stream to be ignored, got complete=%v blocks=%s", complete, blocks)
	}
}

func TestParseClaudeThinkingSSEReconstructsSignedBlock(t *testing.T) {
	raw := []byte("data: {\"type\":\"message_start\"}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n")
	blocks, complete := parseClaudeThinkingSSE(raw)
	if !complete || len(blocks) != 1 {
		t.Fatalf("expected one complete block, got complete=%v blocks=%s", complete, blocks)
	}
	want := json.RawMessage(`{"thinking":"plan","signature":"sig","type":"thinking"}`)
	if !jsonEqual(blocks[0], want) {
		t.Fatalf("unexpected reconstructed block: %s", blocks[0])
	}
}

func TestClaudeSnapshotMismatchShortCircuit(t *testing.T) {
	e := &Engine{claudeSnapshots: map[string]claudeThinkingSnapshot{}}
	e.saveClaudeSnapshot("session", "claude-opus-5-5", byteBlocks(`{"type":"thinking","thinking":"original","signature":"sig"}`))

	body := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"changed","signature":"sig"}]}]}`)
	resp := e.checkClaudeSnapshot("session", "claude-opus-5-5", body)
	if resp == nil || !resp.Terminate || resp.StatusCode != 400 {
		t.Fatalf("expected snapshot mismatch to terminate with 400, got %+v", resp)
	}
	if resp.RuleID != "claude_thinking_block_mismatch" {
		t.Fatalf("unexpected rule: %s", resp.RuleID)
	}
}

func byteBlocks(raw string) []json.RawMessage {
	return []json.RawMessage{json.RawMessage(raw)}
}
