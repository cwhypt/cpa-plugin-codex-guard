package plugin

import (
	"encoding/json"
	"testing"

	"cpa-codex-guard/internal/types"
)

func TestEngineRegister(t *testing.T) {
	engine := NewEngine()
	regBytes, err := engine.HandleMethod(types.MethodPluginRegister, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	var env types.Envelope
	if err := json.Unmarshal(regBytes, &env); err != nil {
		t.Fatalf("unmarshal env failed: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected env.OK=true")
	}

	var reg types.Registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("unmarshal reg failed: %v", err)
	}

	if !reg.Capabilities.RequestInterceptor {
		t.Errorf("expected RequestInterceptor to be true")
	}
	if reg.Metadata.Name != types.PluginID {
		t.Errorf("unexpected name: %s", reg.Metadata.Name)
	}
}
