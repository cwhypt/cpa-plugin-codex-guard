# Configurable session-first account routing

## Execution order

1. CPA authenticates the downstream key and checks model access using its frontend authentication provider (for example, Key Policy X).
2. Guard applies existing request validation and body repair.
3. Existing local arithmetic handling and probe-cache lookup run unchanged. A hit terminates execution before classification, binding creation, Scheduler, or upstream execution.
4. On a miss, Guard uses the host-provided opaque `CallerScope` and explicit session identity to recover a persistent category/account binding. New sessions are classified using configured rules.
5. Guard's Scheduler selects from CPA's eligible candidates. A missing preferred/bound account returns `Handled: false`, allowing CPA's built-in selection and fallback.
6. The after-selection interceptor observes `selected_auth_id`, records a fallback using compare-and-swap, and removes the internal routing header before execution.

Routing is optional and disabled unless `session_routing.enabled` is true. It also works when `probe_cache_enabled` is false. Routing does not change the existing cache's key or sample policy.

## Configuration example

Merge this into the Guard plugin configuration through YAML or the management panel's full configuration editor. Replace the placeholder **auth IDs** with CPA's exact candidate IDs, not raw API keys, key suffixes, email labels, or account indexes. The example rules are illustrative; tune them to confirmed templates before enabling.

```yaml
plugins:
  configs:
    cpa-codex-guard:
      enabled: true
      priority: 5
      session_routing:
        enabled: true
        state_file: data/cpa-session-route-bindings.json
        binding_ttl: 1h
        model_include_regex: '^gpt-'
        default_category: general
        categories:
          general:
            preferred_auth_ids: ['<Codex API-key auth ID>']
          benchmark:
            preferred_auth_ids: ['<guilioenricomeille OAuth auth ID>']
          probe:
            max_priority: 6
            exclude_auth_ids:
              - '<Codex API-key auth ID>'
              - '<guilioenricomeille OAuth auth ID>'
        rules:
          - category: benchmark
            text_all_regex: ['(?i)\bsvg\b', '(?i)\bgenerate\b']
          - category: benchmark
            text_all_regex: ['(?i)\bcandy\b', '(?i)\bdistribution\b']
          - category: probe
            text_all_regex: ['1\s*(?:到|至|[-–])\s*355', '(?:整数|integer)']
```

Rules are ordered: the first match wins. All `text_all_regex` patterns must match the latest user text. Optional `models: [model-a, model-b]` and `user_agent_regex` further constrain a rule, but cannot replace a text condition. Unknown categories, invalid regexes, invalid durations and empty/padded auth IDs reject reconfiguration before replacing the active router.

`preferred_auth_ids` is an ordered preference list. `max_priority` is inclusive, so `6` implements priority `< 7` for CPA's integer priorities. Priority-pool ties use ascending auth ID. OAuth, Codex API-key and compatible-provider candidates are all accepted; CPA remains responsible for model support, cooldown, and excluding already-tried accounts.

## Scheduler priority and authentication

CPA sorts plugin priority **descending**. Priority 5 wins over an effective priority of 0, but loses to a higher-priority Scheduler. Guard declares Scheduler and across-priority candidate support only when session routing is enabled.

Scheduler and frontend authentication are separate capabilities. Key Policy X can continue authenticating and enforcing model access while Guard owns Scheduler. Guard does not require Key Policy-specific `provider` or `key_id` metadata: the actual host execution snapshot carries `caller_scope`, not those authentication fields.

CPA calls only the first eligible Scheduler plugin. `Handled: false` falls back to built-in selection; it does not call Key Policy's Scheduler afterward. Therefore use Key Policy's `routing_mode: host` for this setup. Any legacy `override` group-selection policy needs separate integration before it can coexist with Guard owning Scheduler.

Changing priority alone does not add Scheduler to an older Guard binary. The existing v0.3.8 release predates this implementation. This change is source-only until you build and load a new plugin artifact yourself.

## Sessions, persistence and failure handling

- Caller scope is supplied by CPA after downstream authentication. Guard never reads a client header to establish caller identity. No caller scope means normal host routing.
- Explicit session sources include `Session-Id`, `Session_id`, `X-Session-Id`, `X-Session-Affinity`, body `session_id`, `prompt_cache_key`, conversation ID, and host `canonical_session_id`. No identity is inferred from prompt content. With no session, the host execution request ID isolates the request.
- Category and selected account are shared across models in the same caller/session. An account that cannot serve a later model causes host fallback; the observed fallback becomes the new session preference. This is soft affinity, not strict account isolation.
- Classification uses only the latest user text from Responses `input` or Chat Completions `messages`. Tool-only, missing-user, malformed and unsupported multimodal payloads defer to CPA when there is no existing binding.
- Category is fixed until expiration. Routing requests and account selection refresh the sliding TTL. Cache/local-result hits may refresh an existing explicit session's TTL, but never classify, create a binding, select an account or call upstream. Rule/preference edits apply to new/expired bindings; removing a category makes its bindings defer to CPA.
- The state file contains opaque SHA-256 caller/session keys, categories, auth IDs and expirations, never prompts or API keys. Use a dedicated file, distinct from Guard/probe state files. Back up the state file with the rest of plugin data if bindings must survive machine replacement.
- Each transaction takes a stable sibling `.lock` file lock, reloads the current state, removes expired records, writes and syncs a temporary file, then renames it. Linux/macOS use `flock`; Windows uses `LockFileEx`. Old and new plugin instances coordinate through the same file lock. Fallback writes compare the prior account ID so a stale request cannot blindly replace a newer binding.
- Invalid state is not overwritten. Read/write/lock failures defer to CPA and emit a diagnostic when Guard debug logging is enabled. There is a limit of 10,000 active bindings; new classifications defer when full. The small bounded JSON store serializes disk transactions; a different store is warranted only if measured traffic makes this a bottleneck.
- The internal `X-Cpa-Internal-Session-Route` header is replaced on incoming cache-miss requests. Scheduler requires a matching live host request ID and caller scope. After-selection handling removes the header when present, even when routing is disabled; recording fallback does not depend on the header surviving provider rewrites. It emits no header edits when the marker is absent, avoiding unnecessary host session-metadata reconciliation.

## Verification

Run from the plugin repository:

```sh
go test -race ./...
go vet ./...
```

Regression coverage includes configured classification, concurrent first binding, reload persistence, stale fallback protection, sliding expiration, caller isolation, request-only identity, local/cache short-circuit before routing state, unavailable-target deferral, forged markers, disabled caching, invalid reconfiguration and Responses-Lite body repair.

Host source paths checked for ABI/order compatibility:

- `sdk/api/handlers/handlers.go`: `requestExecutionMetadata`, `requestCallerScope`.
- `sdk/api/handlers/handlers_interceptors.go`: before-selection propagation into `opts.Headers` and after-selection propagation.
- `internal/pluginhost/adapters_interceptors.go`: clear headers before applying replacements.
- `sdk/cliproxy/auth/conductor_selection.go`: candidate filtering, `schedulerOptions`, `pickViaPluginScheduler`.
- `sdk/cliproxy/auth/conductor_execution.go`: `publishSelectedAuthMetadata`, `applyRequestAfterAuthInterceptor`.
- `internal/pluginhost/snapshot.go`, `scheduler.go`, `adapters_auth.go`: descending priority and independent capability selection.

The plugin tests exercise its JSON ABI and lifecycle; host tests verify host behavior separately. Host `internal/pluginhost` scheduler/priority/frontend-authentication tests and `sdk/api/handlers` interceptor/caller-scope tests passed during implementation. The Windows binding-store test executable also cross-compiles successfully (not executed on Windows). These are not a live end-to-end deployment test of the new Scheduler. No production config or binaries are changed by this implementation.
