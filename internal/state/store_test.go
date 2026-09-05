package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreMarkAndCheck(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cpa-guard-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	stateFile := filepath.Join(tmpDir, "guard_state.json")
	store := NewStore(stateFile, 1*time.Second, 1*time.Second)

	itemID := "rs_08ccb93bdcdfbad6016a9b91fc9f388195b666d5e75bc3d65f"
	mockErr := []byte(`{"error":{"code":"thinking_signature_invalid"}}`)

	// 初始检查应不存在
	if invalid, _ := store.IsInvalid(itemID); invalid {
		t.Fatalf("expected item not invalid initially")
	}

	// 标记失效
	store.MarkInvalid(itemID, "session-123", mockErr)

	// 立即检查应为失效
	invalid, payload := store.IsInvalid(itemID)
	if !invalid {
		t.Fatalf("expected item to be invalid")
	}
	if string(payload) != string(mockErr) {
		t.Fatalf("payload mismatch: got %s", string(payload))
	}

	// 重新加载验证持久化
	store2 := NewStore(stateFile, 1*time.Second, 1*time.Second)
	if invalid2, _ := store2.IsInvalid(itemID); !invalid2 {
		t.Fatalf("expected item to persist across store instances")
	}

	// 等待过期
	time.Sleep(1100 * time.Millisecond)
	if invalidAfterTTL, _ := store2.IsInvalid(itemID); invalidAfterTTL {
		t.Fatalf("expected item to expire after TTL")
	}
}

func TestStoreFuzzyCircuitBreaker(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cpa-circuit-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	stateFile := filepath.Join(tmpDir, "circuit_state.json")
	store := NewStore(stateFile, 24*time.Hour, 1*time.Second) // 1s 熔断用于测试

	sessionKeys := []string{"session-abc", "turn-123"}
	inputHashes1 := []string{"h1", "h2", "h3", "h4", "h5", "h6", "h7", "h8", "h9", "h10"}
	mockErr := []byte(`{"error":{"message":"server overloaded","code":"502"}}`)

	// 初始应无熔断
	if tripped, _, _ := store.CheckFuzzyCircuit(sessionKeys, inputHashes1, 0.90); tripped {
		t.Fatalf("expected no circuit initially")
	}

	// 第 1 次错误：记录，但尚未达到连续 2 次
	store.RecordResponseOutcome(sessionKeys, inputHashes1, 502, mockErr, 0.90)
	if tripped, _, _ := store.CheckFuzzyCircuit(sessionKeys, inputHashes1, 0.90); tripped {
		t.Fatalf("expected no circuit after 1st error")
	}

	// 第 2 次错误：追加 1 项新输入（10 项中 10 项全部在，相似度 10/10 = 100% >= 90%）
	inputHashes2 := append(inputHashes1, "h11")
	store.RecordResponseOutcome(sessionKeys, inputHashes2, 502, mockErr, 0.90)

	// 达到连续 2 次相似失败：熔断应触发！
	tripped, code, body := store.CheckFuzzyCircuit(sessionKeys, inputHashes2, 0.90)
	if !tripped {
		t.Fatalf("expected circuit to be tripped after 2nd similar error")
	}
	if code != 502 || string(body) != string(mockErr) {
		t.Errorf("unexpected tripped resp: code=%d body=%s", code, string(body))
	}

	// 另一无关会话发起请求：不应被误伤！
	unrelatedKeys := []string{"session-other"}
	if trippedOther, _, _ := store.CheckFuzzyCircuit(unrelatedKeys, inputHashes2, 0.90); trippedOther {
		t.Fatalf("unrelated session should not be tripped")
	}

	// 等待 1.1s（测试设定的熔断 TTL 过期）
	time.Sleep(1100 * time.Millisecond)
	if trippedAfterTTL, _, _ := store.CheckFuzzyCircuit(sessionKeys, inputHashes2, 0.90); trippedAfterTTL {
		t.Fatalf("expected circuit to expire after TTL")
	}
}
