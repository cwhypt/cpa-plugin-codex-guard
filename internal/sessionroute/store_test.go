package sessionroute

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPersistentBindingAndFallback(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	a := &Store{Path: path, TTL: time.Hour, Now: func() time.Time { return now }}
	b := &Store{Path: path, TTL: time.Hour, Now: func() time.Time { return now }}
	if _, err := a.ClassifyOnce("caller1", "session", "benchmark"); err != nil {
		t.Fatal(err)
	}
	if got, err := b.ClassifyOnce("caller1", "session", "general"); err != nil || got.Category != "benchmark" {
		t.Fatalf("first classification must persist: %+v %v", got, err)
	}
	if got, err := b.ClassifyOnce("caller2", "session", "general"); err != nil || got.Category != "general" {
		t.Fatalf("callers must be isolated: %+v %v", got, err)
	}
	routes := map[string]Route{"benchmark": {PreferredAuthIDs: []string{"oauth"}}}
	id, ok, err := a.Pick("caller1", "session", routes, []Candidate{{ID: "oauth", Priority: 7}, {ID: "other", Priority: 6}})
	if err != nil || !ok || id != "oauth" {
		t.Fatalf("expected OAuth binding: %q %v %v", id, ok, err)
	}
	if _, ok, err := b.Pick("caller1", "session", routes, []Candidate{{ID: "other", Priority: 6}}); err != nil || ok {
		t.Fatalf("unavailable binding must defer: %v %v", ok, err)
	}
	if err := b.RecordFallback("caller1", "session", "oauth", "other"); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordFallback("caller1", "session", "oauth", "stale"); err != nil {
		t.Fatal(err)
	}
	id, ok, err = a.Pick("caller1", "session", routes, []Candidate{{ID: "other", Priority: 6}})
	if err != nil || !ok || id != "other" {
		t.Fatalf("fallback must survive reload and stale update: %q %v %v", id, ok, err)
	}
	now = now.Add(2 * time.Hour)
	if got, err := b.ClassifyOnce("caller1", "session", "general"); err != nil || got.Category != "general" || got.AuthID != "" {
		t.Fatalf("expired binding must be reclassified: %+v %v", got, err)
	}
}

func TestUnavailableBindingUsesOrderedPreferredFallback(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json"), TTL: time.Hour}
	if _, err := s.ClassifyOnce("caller", "session", "general"); err != nil {
		t.Fatal(err)
	}
	routes := map[string]Route{"general": {PreferredAuthIDs: []string{"first", "second"}}}
	if id, ok, err := s.Pick("caller", "session", routes, []Candidate{{ID: "first"}, {ID: "second"}}); err != nil || !ok || id != "first" {
		t.Fatalf("initial selection: %q %v %v", id, ok, err)
	}
	if id, ok, err := s.Pick("caller", "session", routes, []Candidate{{ID: "other", Priority: 99}, {ID: "second", Priority: 1}}); err != nil || !ok || id != "second" {
		t.Fatalf("ordered fallback: %q %v %v", id, ok, err)
	}
	if id, ok, err := s.Pick("caller", "session", routes, []Candidate{{ID: "first"}, {ID: "second"}}); err != nil || !ok || id != "second" {
		t.Fatalf("fallback binding must remain sticky: %q %v %v", id, ok, err)
	}
}

func TestConcurrentFirstClassificationAndPick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	routes := map[string]Route{"a": {PreferredAuthIDs: []string{"A"}}, "b": {PreferredAuthIDs: []string{"B"}}}
	var wg sync.WaitGroup
	results := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := &Store{Path: path, TTL: time.Hour}
			category := "a"
			if i%2 == 1 {
				category = "b"
			}
			if _, err := s.ClassifyOnce("caller", "session", category); err != nil {
				t.Error(err)
				return
			}
			id, ok, err := s.Pick("caller", "session", routes, []Candidate{{ID: "A"}, {ID: "B"}})
			if err != nil || !ok {
				t.Errorf("pick failed: %v %v", ok, err)
				return
			}
			results <- id
		}(i)
	}
	wg.Wait()
	close(results)
	first := ""
	for id := range results {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("first requests selected different accounts: %q and %q", first, id)
		}
	}
}

func TestProbePoolAndCorruptState(t *testing.T) {
	max := 6
	route := Route{MaxPriority: &max, ExcludeAuthIDs: []string{"excluded"}}
	if got, err := choose(route, []Candidate{{ID: "high", Priority: 7}, {ID: "excluded", Priority: 6}, {ID: "low", Priority: 4}, {ID: "preferred", Priority: 5}}, "probe", map[string]poolState{}, time.Now().Add(time.Hour)); err != nil || got != "preferred" {
		t.Fatalf("unexpected probe candidate: %q", got)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Store{Path: path, TTL: time.Hour}
	if _, err := s.ClassifyOnce("caller", "session", "general"); err == nil {
		t.Fatal("invalid state must be rejected")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "not json" {
		t.Fatal("invalid existing state must not be overwritten")
	}
}

func TestBindingSlidingTTL(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Store{Path: filepath.Join(t.TempDir(), "bindings.json"), TTL: time.Hour, Now: func() time.Time { return now }}
	_, _ = s.ClassifyOnce("caller", "session", "general")
	routes := map[string]Route{"general": {PreferredAuthIDs: []string{"api"}}}
	now = now.Add(50 * time.Minute)
	if _, ok, err := s.Pick("caller", "session", routes, []Candidate{{ID: "api"}}); err != nil || !ok {
		t.Fatalf("selection did not refresh TTL: %v %v", ok, err)
	}
	now = now.Add(20 * time.Minute)
	key, _ := Key("caller", "session")
	if binding, ok, err := s.LookupKey(key); err != nil || !ok || binding.AuthID != "api" {
		t.Fatalf("active binding expired: %+v %v %v", binding, ok, err)
	}
	now = now.Add(time.Hour)
	if _, ok, err := s.LookupKey(key); err != nil || ok {
		t.Fatalf("idle binding did not expire: %v %v", ok, err)
	}
}
