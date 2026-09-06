package probecache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreCollectThreeSamplesAndActivate(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cpa-probecache-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	stateFile := filepath.Join(tmpDir, "probe_state.json")
	store := NewStore(stateFile, 24*time.Hour, 3)

	hash1 := "input_hash_ping"

	// 样本 1
	act1 := store.AddSample(hash1, "gpt-4o", "chat", "Hello 1", []byte(`{"reply":"1"}`))
	if act1 {
		t.Fatalf("should not activate on 1 sample")
	}
	if _, ok := store.GetRandomSample(hash1); ok {
		t.Fatalf("should not return sample before activation")
	}

	// 重复样本 1（相同文本不计入）
	actDup := store.AddSample(hash1, "gpt-4o", "chat", "Hello 1", []byte(`{"reply":"1"}`))
	if actDup {
		t.Fatalf("duplicate sample should not activate")
	}

	// 样本 2
	act2 := store.AddSample(hash1, "gpt-4o", "chat", "Hello 2", []byte(`{"reply":"2"}`))
	if act2 {
		t.Fatalf("should not activate on 2 samples")
	}

	// 样本 3 -> 正式激活！
	act3 := store.AddSample(hash1, "gpt-4o", "chat", "Hello 3", []byte(`{"reply":"3"}`))
	if !act3 {
		t.Fatalf("expected store to activate upon reaching 3 distinct samples")
	}

	// 激活后，应当能成功获取到样本
	s, ok := store.GetRandomSample(hash1)
	if !ok || s == nil {
		t.Fatalf("expected to get random sample after activation")
	}

	// 另一个不同的输入 hash2 -> 独立统计，未达标不应激活
	hash2 := "input_hash_pong"
	store.AddSample(hash2, "gpt-4o", "chat", "Pong 1", []byte(`{"reply":"pong"}`))
	if _, ok2 := store.GetRandomSample(hash2); ok2 {
		t.Fatalf("hash2 has only 1 sample, should not be activated")
	}

	// 验证持久化与重新加载
	storeLoaded := NewStore(stateFile, 24*time.Hour, 3)
	sLoaded, okLoaded := storeLoaded.GetRandomSample(hash1)
	if !okLoaded || sLoaded == nil {
		t.Fatalf("expected hash1 to remain active across reload")
	}
}
