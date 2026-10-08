# CPA Codex Guard Plugin (`cpa-codex-guard`)

High-performance native in-process Go C-ABI plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI).

This plugin provides zero-overhead, multi-layer circuit-breaking and intelligent request repair specifically engineered for Codex & ChatGPT backend APIs without requiring any changes to the core CPA gateway.

---

## Features

1. **Static Parameter Blocking (`max_turns`)**
   - Directly rejects requests containing unsupported parameters like `max_turns` before reaching upstream OpenAI/ChatGPT endpoints, returning HTTP `400 Bad Request` instantly in 2ms.

2. **Dynamic Invalid Thinking Signature Defense (`thinking_signature_invalid`)**
   - Automatically learns and extracts invalidated reasoning item IDs (e.g. `rs_...`) from upstream 400 responses.
   - Atomically records them to an isolated persistent JSON state file with 24h TTL.
   - Short-circuits subsequent retries carrying the same dead signature to terminate endless client-side retry loops.

3. **Intelligent Protocol Auto-Fix (`Responses-Lite`)**
   - Detects `X-OpenAI-Internal-Codex-Responses-Lite: true` headers missing the mandatory `reasoning.context = "all_turns"` parameter.
   - Automatically injects and normalizes the payload on-the-fly, converting upstream 400 rejection into successful execution!

4. **Session-Level Fuzzy Matching Circuit Breaker**
   - Anchored on `session_id` or `root_turn_id` (model-agnostic).
   - Automatically tracks consecutive failures: if a session encounters **2 consecutive errors** with $\ge 90\%$ request body context similarity, it trips a **3-hour circuit breaker**.
   - Subsequent similar requests are immediately short-circuited at the proxy level returning the cached upstream error.
   - Other sessions and clean new windows are completely unaffected!

5. **Exact Claude Request Guards**
   - Rejects `claude-opus-5-5` requests that send `thinking.type=disabled`; the model requires adaptive thinking plus `output_config.effort`.
   - Rejects `claude-sonnet-5-5` requests that send `thinking.type=disabled`; the model requires `thinking.type=between_tools` when thinking is turned off.
   - Rejects Claude tools whose present `input_schema` is `null` or another non-object JSON value.
   - Returns the observed upstream-compatible HTTP 400 body before any provider call. It does not rewrite requests or handle invalid model names.

---

## Directory Structure

```text
.
├── Makefile              # Cross-compilation and build targets
├── cmd
│   └── cpa-codex-guard
│       └── main.go       # C-ABI export bridge (cliproxy_plugin_init)
├── internal
│   ├── guard
│   │   ├── checker.go    # Inspection, Auto-Fix, and short-circuit evaluation
│   │   └── checker_test.go
│   ├── plugin
│   │   ├── config.go     # Configuration parsing & defaults
│   │   ├── engine.go     # Plugin lifecycle and method dispatcher
│   │   └── engine_test.go
│   ├── state
│   │   ├── store.go      # Concurrent atomic persistence and overlap hashing
│   │   └── store_test.go
│   └── types
│       ├── types.go      # CPA plugin ABI types and contract definitions
│       └── types_test.go
├── simulate_runner.go    # End-to-end dlopen simulation runner
└── go.mod
```

---

## Build

```bash
make test
make build
```

The output dynamic library will be built to:
```text
plugins/linux/arm64/cpa-codex-guard-v0.3.3.so
```

---

## Configuration (`config.yaml`)

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-codex-guard:
      enabled: true
      state_file: "data/cpa-codex-guard-state.json"
      ttl: "24h"
      circuit_ttl: "3h"
      block_max_turns: true
      block_invalid_signatures: true
      block_invalid_claude_requests: true
      autofix_responses_lite: true
      fuzzy_circuit_breaker: true
      fuzzy_similarity_threshold: 0.90
```

---

## License

MIT
