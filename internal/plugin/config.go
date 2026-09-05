package plugin

import (
	"time"
)

type Config struct {
	StateFile              string  `json:"state_file" yaml:"state_file"`
	TTLStr                 string  `json:"ttl" yaml:"ttl"`
	CircuitTTLStr          string  `json:"circuit_ttl" yaml:"circuit_ttl"`
	BlockMaxTurns          *bool   `json:"block_max_turns" yaml:"block_max_turns"`
	BlockInvalidSignatures *bool   `json:"block_invalid_signatures" yaml:"block_invalid_signatures"`
	AutoFixResponsesLite   *bool   `json:"autofix_responses_lite" yaml:"autofix_responses_lite"`
	FuzzyCircuitBreaker    *bool   `json:"fuzzy_circuit_breaker" yaml:"fuzzy_circuit_breaker"`
	SimilarityThreshold    float64 `json:"fuzzy_similarity_threshold" yaml:"fuzzy_similarity_threshold"`
}

func DefaultConfig() *Config {
	t := true
	return &Config{
		StateFile:              "data/cpa-codex-guard-state.json",
		TTLStr:                 "24h",
		CircuitTTLStr:          "3h",
		BlockMaxTurns:          &t,
		BlockInvalidSignatures: &t,
		AutoFixResponsesLite:   &t,
		FuzzyCircuitBreaker:    &t,
		SimilarityThreshold:    0.90,
	}
}

func (c *Config) ParseTTL() time.Duration {
	if c.TTLStr == "" {
		return 24 * time.Hour
	}
	d, err := time.ParseDuration(c.TTLStr)
	if err != nil || d <= 0 {
		return 24 * time.Hour
	}
	return d
}

func (c *Config) ParseCircuitTTL() time.Duration {
	if c.CircuitTTLStr == "" {
		return 3 * time.Hour
	}
	d, err := time.ParseDuration(c.CircuitTTLStr)
	if err != nil || d <= 0 {
		return 3 * time.Hour
	}
	return d
}

func (c *Config) IsBlockMaxTurns() bool {
	if c.BlockMaxTurns == nil {
		return true
	}
	return *c.BlockMaxTurns
}

func (c *Config) IsBlockInvalidSignatures() bool {
	if c.BlockInvalidSignatures == nil {
		return true
	}
	return *c.BlockInvalidSignatures
}

func (c *Config) IsAutoFixResponsesLite() bool {
	if c.AutoFixResponsesLite == nil {
		return true
	}
	return *c.AutoFixResponsesLite
}

func (c *Config) IsFuzzyCircuitBreaker() bool {
	if c.FuzzyCircuitBreaker == nil {
		return true
	}
	return *c.FuzzyCircuitBreaker
}

func (c *Config) GetSimilarityThreshold() float64 {
	if c.SimilarityThreshold <= 0 || c.SimilarityThreshold > 1.0 {
		return 0.90
	}
	return c.SimilarityThreshold
}
