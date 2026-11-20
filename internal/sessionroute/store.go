package sessionroute

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxBindings = 10000
const maxPoolEntries = 1024
const maxWeight = 1000000 // CPA's credential weight limit.

type Binding struct {
	Category  string    `json:"category"`
	AuthID    string    `json:"auth_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Candidate struct {
	ID       string
	Priority int
	Weight   *int64 // nil means CPA's default weight of one; zero disables selection.
}

type poolCredit struct {
	Weight  int64 `json:"weight"`
	Current int64 `json:"current"`
}

type poolState struct {
	Credits   map[string]poolCredit `json:"credits"`
	ExpiresAt time.Time             `json:"expires_at"`
}

type routingState struct {
	Version  int                  `json:"version"`
	Bindings map[string]Binding   `json:"bindings"`
	Pools    map[string]poolState `json:"pools"`
}

// Store shares bindings and rotation credits across plugin instances/processes.
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

func decodeRoutingState(data []byte) (routingState, error) {
	state := routingState{Version: 1, Bindings: make(map[string]Binding), Pools: make(map[string]poolState)}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return state, errors.New("invalid routing state; refusing to overwrite")
	}
	if _, versioned := root["version"]; versioned {
		if json.Unmarshal(data, &state) != nil || state.Version != 1 || state.Bindings == nil {
			return state, errors.New("invalid routing state version; refusing to overwrite")
		}
	} else if json.Unmarshal(data, &state.Bindings) != nil {
		return state, errors.New("invalid legacy routing state; refusing to overwrite")
	}
	for key, binding := range state.Bindings {
		if !validKey(key) || binding.Category == "" || binding.ExpiresAt.IsZero() {
			return state, errors.New("invalid routing binding; refusing to overwrite")
		}
	}
	if state.Pools == nil {
		state.Pools = make(map[string]poolState)
	}
	for key, pool := range state.Pools {
		if !validKey(key) || pool.ExpiresAt.IsZero() || len(pool.Credits) > maxPoolEntries {
			return state, errors.New("invalid routing pool; refusing to overwrite")
		}
		for id, credit := range pool.Credits {
			if id == "" || credit.Weight <= 0 || credit.Weight > maxWeight || credit.Current < -(1<<60) || credit.Current > 1<<60 {
				return state, errors.New("invalid routing credit; refusing to overwrite")
			}
		}
	}
	return state, nil
}

func (s *Store) update(fn func(*routingState, time.Time) error) error {
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
	state := routingState{Version: 1, Bindings: make(map[string]Binding), Pools: make(map[string]poolState)}
	data, err := os.ReadFile(s.Path)
	if err == nil {
		state, err = decodeRoutingState(data)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Expiration, classification, renewal and account reservation use one clock
	// snapshot under the same lock. A category cannot expire between these steps.
	now := s.now()
	for key, binding := range state.Bindings {
		if !binding.ExpiresAt.After(now) {
			delete(state.Bindings, key)
		}
	}
	for key, pool := range state.Pools {
		if !pool.ExpiresAt.After(now) {
			delete(state.Pools, key)
		}
	}
	if err := fn(&state, now); err != nil {
		return err
	}
	data, err = json.Marshal(state)
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

// Resolve classifies only when the binding is absent/expired, inside the lock.
func (s *Store) Resolve(caller, session string, classify func() (string, bool)) (Binding, bool, error) {
	key, err := bindingKey(caller, session)
	if err != nil {
		return Binding{}, false, err
	}
	var result Binding
	var resolved bool
	err = s.update(func(state *routingState, now time.Time) error {
		result = state.Bindings[key]
		if result.Category == "" {
			category, ok := classify()
			if !ok || strings.TrimSpace(category) == "" {
				return nil
			}
			if len(state.Bindings) >= maxBindings {
				return errors.New("session routing state capacity reached")
			}
			result.Category = category
		}
		result.ExpiresAt = now.Add(s.TTL)
		state.Bindings[key] = result
		resolved = true
		return nil
	})
	return result, resolved, err
}

func (s *Store) ClassifyOnce(caller, session, category string) (Binding, error) {
	if strings.TrimSpace(category) == "" {
		return Binding{}, errors.New("category is required")
	}
	binding, _, err := s.Resolve(caller, session, func() (string, bool) { return category, true })
	return binding, err
}

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
	err := s.update(func(state *routingState, now time.Time) error {
		binding, ok := state.Bindings[key]
		if !ok {
			return nil
		}
		route, ok := categories[binding.Category]
		if !ok {
			return nil
		}
		binding.ExpiresAt = now.Add(s.TTL)
		state.Bindings[key] = binding
		if binding.AuthID != "" {
			for _, candidate := range candidates {
				if candidate.ID == binding.AuthID && candidate.weight() > 0 {
					selected = candidate.ID
					break
				}
			}
			return nil
		}
		var err error
		selected, err = choose(route, candidates, binding.Category, state.Pools, now.Add(s.TTL))
		if err != nil {
			return err
		}
		if selected != "" {
			binding.AuthID = selected
			state.Bindings[key] = binding
		}
		return nil
	})
	return selected, selected != "", err
}

// PickProtectedFallback preserves priority/weight semantics after reserved auths
// are removed. Returning to the host with the unfiltered pool would undo that
// protection. Allocation and any existing binding update share one transaction.
func (s *Store) PickProtectedFallback(key, model string, candidates []Candidate) (string, bool, error) {
	if key != "" && !validKey(key) {
		return "", false, errors.New("invalid fallback binding key")
	}
	var selected string
	err := s.update(func(state *routingState, now time.Time) error {
		if binding, ok := state.Bindings[key]; ok && binding.AuthID != "" {
			for _, candidate := range candidates {
				if candidate.ID == binding.AuthID && candidate.weight() > 0 {
					selected = candidate.ID
					binding.ExpiresAt = now.Add(s.TTL)
					state.Bindings[key] = binding
					return nil
				}
			}
		}
		max := math.MaxInt
		var err error
		selected, err = choose(Route{MaxPriority: &max}, candidates, "protected-fallback:"+model, state.Pools, now.Add(s.TTL))
		if err != nil {
			return err
		}
		if binding, ok := state.Bindings[key]; ok && selected != "" {
			binding.AuthID = selected
			binding.ExpiresAt = now.Add(s.TTL)
			state.Bindings[key] = binding
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
	err := s.update(func(state *routingState, _ time.Time) error { result, ok = state.Bindings[key]; return nil })
	return result, ok, err
}

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
	return s.update(func(state *routingState, now time.Time) error {
		if binding, ok := state.Bindings[key]; ok {
			binding.ExpiresAt = now.Add(s.TTL)
			state.Bindings[key] = binding
		}
		return nil
	})
}

func (c Candidate) weight() int64 {
	if c.Weight == nil {
		return 1
	}
	if *c.Weight < 0 || *c.Weight > maxWeight {
		return 0
	}
	return *c.Weight
}

func choose(route Route, candidates []Candidate, category string, pools map[string]poolState, expires time.Time) (string, error) {
	byID := make(map[string]Candidate, len(candidates))
	for _, c := range candidates {
		if c.ID != "" && c.weight() > 0 && !contains(route.ExcludeAuthIDs, c.ID) && (route.MaxPriority == nil || c.Priority <= *route.MaxPriority) {
			byID[c.ID] = c
		}
	}
	for _, id := range route.PreferredAuthIDs {
		if _, ok := byID[id]; ok {
			return id, nil
		}
	}
	if route.MaxPriority == nil || len(byID) == 0 {
		return "", nil
	}
	eligible := make([]Candidate, 0, len(byID))
	for _, c := range byID {
		eligible = append(eligible, c)
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Priority != eligible[j].Priority {
			return eligible[i].Priority > eligible[j].Priority
		}
		return eligible[i].ID < eligible[j].ID
	})
	end := 1
	for end < len(eligible) && eligible[end].Priority == eligible[0].Priority {
		end++
	}
	eligible = eligible[:end]
	poolKey, _ := bindingKey(category, strconv.Itoa(eligible[0].Priority))
	if len(eligible) > maxPoolEntries || len(pools) >= maxPoolEntries && pools[poolKey].Credits == nil {
		return "", errors.New("routing pool capacity reached")
	}
	pool := pools[poolKey]
	if pool.Credits == nil {
		pool.Credits = make(map[string]poolCredit)
	}
	// A temporary candidate subset is not a configuration change: keep absent
	// accounts' credits, as CPA's smooth weighted selector does.
	for _, c := range eligible {
		if old, ok := pool.Credits[c.ID]; ok && old.Weight != c.weight() {
			for id, credit := range pool.Credits {
				credit.Current = 0
				pool.Credits[id] = credit
			}
			break
		}
	}
	var total int64
	best := eligible[0].ID
	for _, c := range eligible {
		credit := pool.Credits[c.ID]
		credit.Weight = c.weight()
		credit.Current += credit.Weight
		pool.Credits[c.ID] = credit
		total += credit.Weight
		if credit.Current > pool.Credits[best].Current {
			best = c.ID
		}
	}
	credit := pool.Credits[best]
	credit.Current -= total
	pool.Credits[best] = credit
	if len(pool.Credits) > maxPoolEntries {
		active := make(map[string]bool, len(eligible))
		for _, candidate := range eligible {
			active[candidate.ID] = true
		}
		for id := range pool.Credits {
			if !active[id] {
				delete(pool.Credits, id)
			}
		}
	}
	pool.ExpiresAt = expires
	pools[poolKey] = pool
	return best, nil
}

func (s *Store) RecordFallback(caller, session, expectedAuthID, actualAuthID string) error {
	key, err := bindingKey(caller, session)
	if err != nil {
		return err
	}
	return s.RecordFallbackKey(key, expectedAuthID, actualAuthID)
}

func (s *Store) RecordFallbackKey(key, expectedAuthID, actualAuthID string) error {
	if !validKey(key) || strings.TrimSpace(actualAuthID) == "" {
		return fmt.Errorf("invalid fallback identity")
	}
	return s.update(func(state *routingState, now time.Time) error {
		if binding, ok := state.Bindings[key]; ok && binding.AuthID == expectedAuthID {
			binding.AuthID = actualAuthID
			binding.ExpiresAt = now.Add(s.TTL)
			state.Bindings[key] = binding
		}
		return nil
	})
}
