package sessionroute

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxBindings = 10000

type Binding struct {
	Category  string    `json:"category"`
	AuthID    string    `json:"auth_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Candidate struct {
	ID       string
	Priority int
}

// Store shares state across plugin versions and processes using an advisory lock.
type Store struct {
	Path string
	TTL  time.Duration
	Now  func() time.Time
}

func bindingKey(caller, session string) (string, error) {
	if strings.TrimSpace(caller) == "" || strings.TrimSpace(session) == "" {
		return "", errors.New("caller and session are required")
	}
	encoded, _ := json.Marshal([]string{caller, session})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Key returns an opaque binding key without exposing the caller or session ID.
func Key(caller, session string) (string, error) { return bindingKey(caller, session) }

func validKey(key string) bool {
	decoded, err := hex.DecodeString(key)
	return err == nil && len(decoded) == sha256.Size
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) update(fn func(map[string]Binding) error) error {
	if s.Path == "" || s.TTL <= 0 {
		return errors.New("state file and positive TTL are required")
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	unlock, err := lockStateFile(lock)
	if err != nil {
		return err
	}
	defer unlock()
	bindings := make(map[string]Binding)
	data, err := os.ReadFile(s.Path)
	if err == nil {
		if err := json.Unmarshal(data, &bindings); err != nil || bindings == nil {
			return errors.New("invalid routing state; refusing to overwrite")
		}
		for key, binding := range bindings {
			if !validKey(key) || binding.Category == "" || binding.ExpiresAt.IsZero() {
				return errors.New("invalid routing binding; refusing to overwrite")
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	now := s.now()
	for key, binding := range bindings {
		if !binding.ExpiresAt.After(now) {
			delete(bindings, key)
		}
	}
	if err := fn(bindings); err != nil {
		return err
	}
	data, err = json.Marshal(bindings)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".session-route-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// ClassifyOnce persists the first category, refreshing the sliding expiration.
func (s *Store) ClassifyOnce(caller, session, category string) (Binding, error) {
	key, err := bindingKey(caller, session)
	if err != nil || strings.TrimSpace(category) == "" {
		return Binding{}, errors.New("caller, session and category are required")
	}
	var result Binding
	err = s.update(func(bindings map[string]Binding) error {
		result = bindings[key]
		if result.Category == "" {
			if len(bindings) >= maxBindings {
				return errors.New("session routing state capacity reached")
			}
			result.Category = category
		}
		result.ExpiresAt = s.now().Add(s.TTL)
		bindings[key] = result
		return nil
	})
	return result, err
}

// Pick reserves one available candidate atomically, or defers to the host.
// A previously bound but unavailable auth is not silently replaced here.
func (s *Store) Pick(caller, session string, categories map[string]Route, candidates []Candidate) (string, bool, error) {
	key, err := bindingKey(caller, session)
	if err != nil {
		return "", false, err
	}
	return s.PickKey(key, categories, candidates)
}

func (s *Store) PickKey(key string, categories map[string]Route, candidates []Candidate) (string, bool, error) {
	if !validKey(key) {
		return "", false, errors.New("invalid binding key")
	}
	var selected string
	err := s.update(func(bindings map[string]Binding) error {
		binding, ok := bindings[key]
		if !ok {
			return nil
		}
		route, ok := categories[binding.Category]
		if !ok {
			return nil
		}
		binding.ExpiresAt = s.now().Add(s.TTL)
		bindings[key] = binding
		if binding.AuthID != "" {
			for _, candidate := range candidates {
				if candidate.ID == binding.AuthID {
					selected = candidate.ID
					return nil
				}
			}
			return nil
		}
		selected = choose(route, candidates)
		if selected != "" {
			binding.AuthID = selected
			bindings[key] = binding
		}
		return nil
	})
	return selected, selected != "", err
}

func (s *Store) LookupKey(key string) (Binding, bool, error) {
	if !validKey(key) {
		return Binding{}, false, errors.New("invalid binding key")
	}
	var result Binding
	var ok bool
	err := s.update(func(bindings map[string]Binding) error {
		result, ok = bindings[key]
		return nil
	})
	return result, ok, err
}

// TouchExistingKey refreshes activity without creating a state file or binding.
func (s *Store) TouchExistingKey(key string) error {
	if !validKey(key) {
		return errors.New("invalid binding key")
	}
	if _, err := os.Stat(s.Path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return s.update(func(bindings map[string]Binding) error {
		if binding, ok := bindings[key]; ok {
			binding.ExpiresAt = s.now().Add(s.TTL)
			bindings[key] = binding
		}
		return nil
	})
}

func choose(route Route, candidates []Candidate) string {
	byID := make(map[string]Candidate, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID != "" && !contains(route.ExcludeAuthIDs, candidate.ID) && (route.MaxPriority == nil || candidate.Priority <= *route.MaxPriority) {
			byID[candidate.ID] = candidate
		}
	}
	for _, id := range route.PreferredAuthIDs {
		if _, ok := byID[id]; ok {
			return id
		}
	}
	if route.MaxPriority == nil {
		return ""
	}
	eligible := make([]Candidate, 0, len(byID))
	for _, candidate := range byID {
		eligible = append(eligible, candidate)
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Priority != eligible[j].Priority {
			return eligible[i].Priority > eligible[j].Priority
		}
		return eligible[i].ID < eligible[j].ID
	})
	if len(eligible) == 0 {
		return ""
	}
	return eligible[0].ID
}

// RecordFallback uses compare-and-swap so stale completions cannot overwrite a
// binding changed concurrently by another request or plugin instance.
func (s *Store) RecordFallback(caller, session, expectedAuthID, actualAuthID string) error {
	key, err := bindingKey(caller, session)
	if err != nil || strings.TrimSpace(actualAuthID) == "" {
		return fmt.Errorf("invalid fallback identity")
	}
	return s.RecordFallbackKey(key, expectedAuthID, actualAuthID)
}

func (s *Store) RecordFallbackKey(key, expectedAuthID, actualAuthID string) error {
	if !validKey(key) || strings.TrimSpace(actualAuthID) == "" {
		return errors.New("invalid fallback identity")
	}
	return s.update(func(bindings map[string]Binding) error {
		if binding, ok := bindings[key]; ok && binding.AuthID == expectedAuthID {
			binding.AuthID = actualAuthID
			binding.ExpiresAt = s.now().Add(s.TTL)
			bindings[key] = binding
		}
		return nil
	})
}
