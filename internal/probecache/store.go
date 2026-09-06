package probecache

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Sample struct {
	Text        string          `json:"text"`
	RawResponse json.RawMessage `json:"raw_response"`
	CreatedAt   time.Time       `json:"created_at"`
}

type ProbeEntry struct {
	InputHash  string    `json:"input_hash"`
	Model      string    `json:"model"`
	Format     string    `json:"format"`
	CreatedAt  time.Time `json:"created_at"`
	ExpireAt   time.Time `json:"expire_at"`
	Activated  bool      `json:"activated"`
	HitCount   int       `json:"hit_count"`
	Samples    []Sample  `json:"samples"`
}

type StateData struct {
	Version   int                   `json:"version"`
	UpdatedAt time.Time             `json:"updated_at"`
	Entries   map[string]ProbeEntry `json:"entries"`
}

type Store struct {
	mu         sync.RWMutex
	filePath   string
	ttl        time.Duration
	minSamples int
	entries    map[string]ProbeEntry
	isDirty    bool
}

func NewStore(filePath string, ttl time.Duration, minSamples int) *Store {
	if ttl <= 0 {
		ttl = 72 * time.Hour
	}
	if minSamples <= 0 {
		minSamples = 3
	}
	s := &Store{
		filePath:   filePath,
		ttl:        ttl,
		minSamples: minSamples,
		entries:    make(map[string]ProbeEntry),
	}
	_ = s.load()
	return s
}

func (s *Store) AddSample(inputHash, model, format, text string, rawResponse []byte) bool {
	if inputHash == "" || text == "" || len(rawResponse) == 0 {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	entry, exists := s.entries[inputHash]
	if !exists {
		entry = ProbeEntry{
			InputHash: inputHash,
			Model:     model,
			Format:    format,
			CreatedAt: now,
			ExpireAt:  now.Add(s.ttl),
			Activated: false,
			Samples:   make([]Sample, 0, s.minSamples),
		}
	}

	// 只要未收集满 minSamples，就追加样本（无需输出内容不同）
	if len(entry.Samples) < s.minSamples {
		entry.Samples = append(entry.Samples, Sample{
			Text:        text,
			RawResponse: json.RawMessage(rawResponse),
			CreatedAt:   now,
		})
	}

	if len(entry.Samples) >= s.minSamples {
		entry.Activated = true
	}

	s.entries[inputHash] = entry
	s.isDirty = true
	_ = s.saveLocked()

	return entry.Activated
}

func (s *Store) GetRandomSample(inputHash string) (*Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[inputHash]
	if !ok || !entry.Activated || len(entry.Samples) == 0 {
		return nil, false
	}

	now := time.Now()
	if now.After(entry.ExpireAt) {
		delete(s.entries, inputHash)
		s.isDirty = true
		return nil, false
	}

	// 随机选取一个样本
	idxBig, err := rand.Int(rand.Reader, big.NewInt(int64(len(entry.Samples))))
	var idx int
	if err == nil {
		idx = int(idxBig.Int64())
	} else {
		idx = 0
	}

	entry.HitCount++
	s.entries[inputHash] = entry
	s.isDirty = true

	chosen := entry.Samples[idx]
	return &chosen, true
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
	for k, v := range state.Entries {
		if now.Before(v.ExpireAt) {
			s.entries[k] = v
		}
	}
	return nil
}

func (s *Store) saveLocked() error {
	if !s.isDirty || s.filePath == "" {
		return nil
	}

	now := time.Now()
	active := make(map[string]ProbeEntry)
	for k, v := range s.entries {
		if now.Before(v.ExpireAt) {
			active[k] = v
		}
	}

	s.entries = active
	s.isDirty = false

	state := StateData{
		Version:   1,
		UpdatedAt: now,
		Entries:   active,
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
