package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type InvalidItem struct {
	SessionID    string          `json:"session_id,omitempty"`
	FirstSeen    time.Time       `json:"first_seen"`
	ExpireAt     time.Time       `json:"expire_at"`
	ErrorPayload json.RawMessage `json:"error_payload"`
}

type SessionCircuit struct {
	SessionKey       string          `json:"session_key"`
	ErrorCount       int             `json:"error_count"`
	LastInputHashes  []string        `json:"last_input_hashes"`
	LastStatusCode   int             `json:"last_status_code"`
	LastErrorPayload json.RawMessage `json:"last_error_payload"`
	CircuitExpireAt  time.Time       `json:"circuit_expire_at,omitempty"`
}

type StateData struct {
	Version         int                       `json:"version"`
	UpdatedAt       time.Time                 `json:"updated_at"`
	InvalidItems    map[string]InvalidItem    `json:"invalid_items"`
	CircuitSessions map[string]SessionCircuit `json:"circuit_sessions,omitempty"`
}

type Store struct {
	mu         sync.RWMutex
	filePath   string
	ttl        time.Duration
	circuitTTL time.Duration
	items      map[string]InvalidItem
	circuits   map[string]SessionCircuit
	isDirty    bool
}

func NewStore(filePath string, ttl, circuitTTL time.Duration) *Store {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if circuitTTL <= 0 {
		circuitTTL = 3 * time.Hour
	}
	s := &Store{
		filePath:   filePath,
		ttl:        ttl,
		circuitTTL: circuitTTL,
		items:      make(map[string]InvalidItem),
		circuits:   make(map[string]SessionCircuit),
	}
	_ = s.load()
	return s
}

func (s *Store) IsInvalid(itemID string) (bool, []byte) {
	s.mu.RLock()
	item, ok := s.items[itemID]
	s.mu.RUnlock()

	if !ok {
		return false, nil
	}

	if time.Now().After(item.ExpireAt) {
		s.mu.Lock()
		delete(s.items, itemID)
		s.isDirty = true
		s.mu.Unlock()
		return false, nil
	}

	return true, []byte(item.ErrorPayload)
}

func (s *Store) MarkInvalid(itemID, sessionID string, errorPayload []byte) {
	if itemID == "" {
		return
	}
	now := time.Now()
	expireAt := now.Add(s.ttl)

	s.mu.Lock()
	s.items[itemID] = InvalidItem{
		SessionID:    sessionID,
		FirstSeen:    now,
		ExpireAt:     expireAt,
		ErrorPayload: json.RawMessage(errorPayload),
	}
	s.isDirty = true
	s.mu.Unlock()

	_ = s.save()
}

// CheckFuzzyCircuit 检查会话是否处于 3h 熔断且当前输入相似度 >= 90%
func (s *Store) CheckFuzzyCircuit(keys []string, currentHashes []string, threshold float64) (bool, int, []byte) {
	if len(keys) == 0 || len(currentHashes) == 0 {
		return false, 0, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	for _, k := range keys {
		if k == "" {
			continue
		}
		c, ok := s.circuits[k]
		if !ok {
			continue
		}

		// 检查是否已激活熔断且未过期
		if !c.CircuitExpireAt.IsZero() && now.Before(c.CircuitExpireAt) {
			sim := computeOverlap(currentHashes, c.LastInputHashes)
			if sim >= threshold {
				return true, c.LastStatusCode, []byte(c.LastErrorPayload)
			}
		}
	}

	return false, 0, nil
}

// RecordResponseOutcome 记录请求结果，用于熔断判定
func (s *Store) RecordResponseOutcome(keys []string, currentHashes []string, statusCode int, respPayload []byte, threshold float64) {
	if len(keys) == 0 || statusCode == 0 {
		return
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	// 200 成功：重置连续错误计数
	if statusCode >= 200 && statusCode < 300 {
		for _, k := range keys {
			if k == "" {
				continue
			}
			if c, ok := s.circuits[k]; ok {
				c.ErrorCount = 0
				c.CircuitExpireAt = time.Time{}
				s.circuits[k] = c
				s.isDirty = true
			}
		}
		return
	}

	// 发生非 200 错误
	for _, k := range keys {
		if k == "" {
			continue
		}
		c, ok := s.circuits[k]
		if !ok {
			s.circuits[k] = SessionCircuit{
				SessionKey:       k,
				ErrorCount:       1,
				LastInputHashes:  currentHashes,
				LastStatusCode:   statusCode,
				LastErrorPayload: json.RawMessage(respPayload),
			}
			s.isDirty = true
			continue
		}

		// 计算与上次失败的相似度
		sim := computeOverlap(currentHashes, c.LastInputHashes)
		if sim >= threshold {
			c.ErrorCount++
		} else {
			c.ErrorCount = 1
		}

		c.LastInputHashes = currentHashes
		c.LastStatusCode = statusCode
		c.LastErrorPayload = json.RawMessage(respPayload)

		// 达到连续 2 次相似失败：触发 3h 熔断
		if c.ErrorCount >= 2 {
			c.CircuitExpireAt = now.Add(s.circuitTTL)
		}

		s.circuits[k] = c
		s.isDirty = true
	}

	_ = s.saveLocked()
}

func computeOverlap(current, last []string) float64 {
	if len(last) == 0 {
		return 0
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, h := range current {
		currentSet[h] = struct{}{}
	}

	shared := 0
	for _, h := range last {
		if _, exists := currentSet[h]; exists {
			shared++
		}
	}

	return float64(shared) / float64(len(last))
}

func HashJSON(data any) string {
	b, _ := json.Marshal(data)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.filePath == "" {
		return nil
	}
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var state StateData
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	now := time.Now()
	for k, v := range state.InvalidItems {
		if now.Before(v.ExpireAt) {
			s.items[k] = v
		}
	}
	for k, v := range state.CircuitSessions {
		if (v.CircuitExpireAt.IsZero() || now.Before(v.CircuitExpireAt)) && v.LastStatusCode != 0 {
			s.circuits[k] = v
		}
	}
	return nil
}

func (s *Store) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if !s.isDirty || s.filePath == "" {
		return nil
	}

	now := time.Now()
	activeItems := make(map[string]InvalidItem)
	for k, v := range s.items {
		if now.Before(v.ExpireAt) {
			activeItems[k] = v
		}
	}
	activeCircuits := make(map[string]SessionCircuit)
	for k, v := range s.circuits {
		if (v.CircuitExpireAt.IsZero() || now.Before(v.CircuitExpireAt)) && v.LastStatusCode != 0 {
			activeCircuits[k] = v
		}
	}

	s.items = activeItems
	s.circuits = activeCircuits
	s.isDirty = false

	state := StateData{
		Version:         1,
		UpdatedAt:       now,
		InvalidItems:    activeItems,
		CircuitSessions: activeCircuits,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.filePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	tmpPath := fmt.Sprintf("%s.tmp-%d-%d", s.filePath, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.filePath)
}
