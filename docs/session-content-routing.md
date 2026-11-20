# Configurable session-first account routing

## Execution order

1. CPA authenticates the downstream key and checks model access using its frontend authentication provider (for example, Key Policy X).
2. Guard applies existing request validation and body repair.
3. Existing local arithmetic handling and probe-cache lookup run unchanged. A hit terminates execution before classification, binding creation, Scheduler, or upstream execution.
4. On a miss, Guard uses the host-provided opaque `CallerScope` and explicit session identity to recover a persistent category/account binding. New sessions are classified using configured rules.
5. Guard's Scheduler selects from CPA's eligible candidates. A missing preferred/bound account returns `Handled: false`, allowing CPA's built-in selection and fallback.
6. The after-selection interceptor observes `selected_auth_id`, records a fallback using compare-and-swap, and removes the internal routing header before execution.

Routing is optional and disabled unless `session_routing.enabled` is true. It also works when `probe_cache_enabled` is false. Routing does not change the existing cache's key or sample policy.

The additional reserved-quality-account policy, measured thresholds, full configuration fragment and protected fallback behavior are documented in [quality-account-routing-2026-11-20.md](quality-account-routing-2026-11-20.md). It splits ordinary astra/sol medium-or-higher traffic by substantive user/context length, while preserving sticky short follow-ups.

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
            models: [gpt-6.1-sol]
            user_agent_regex: '^<confirmed-probe-client>/'
            standalone: true
            disallow_tools: true
            max_input_bytes: 2048
            text_all_regex: ['1\s*(?:到|至|[-–])\s*355', '(?:整数|integer)']
```

Rules are ordered: the first match wins. All `text_all_regex` patterns must match the **first effective user text** in the first observed cache-miss request for an unbound/expired session. Optional `models: [model-a, model-b]`, per-rule `model_include_regex`, `reasoning_efforts`, `user_agent_regex` and `caller_scopes: [<opaque CPA caller scope>]` further constrain a rule. A structural length rule can omit text regexes only when it supplies a model constraint, explicit effort list, and positive `min_user_chars` or `min_context_chars`; these are Unicode-character minima, not request-byte sizes. All conditions within a rule are ANDed; use two rules for OR alternatives. Unknown categories, invalid regexes, invalid durations and empty/padded auth IDs reject reconfiguration before replacing the active router.

Rules targeting a `max_priority` pool must specify `standalone: true`, a positive `max_input_bytes` (whole request size), and either `user_agent_regex` or nonempty `caller_scopes`. Pool categories cannot be the default category. `standalone` permits exactly one user message plus optional system/developer messages, and rejects assistant/tool/unknown conversation items, tools/tool choice, `previous_response_id` and conversation references. `disallow_tools` can also constrain non-pool rules. A UA is a configurable client hint, not proof of caller identity; use `caller_scopes` when the rule must be limited to particular authenticated callers. Tighten text regexes to the entire confirmed template instead of copying the illustrative keyword patterns into production.

`preferred_auth_ids` is an ordered preference list. `max_priority` is inclusive, so `6` implements priority `< 7` for CPA's integer priorities. New pool bindings use CPA-style **smooth weighted round-robin within the highest available priority tier**; lower tiers are used when the higher tier is absent. Weight comes from candidate `Attributes["weight"]`, then metadata if present, defaulting to one; zero/invalid weights are excluded. Equal credits use ascending auth ID for a deterministic tie. Existing session picks do not consume rotation credits again. Rotation credits are persisted atomically with account binding and shared by category/priority across plugin instances; temporary candidate subsets retain absent accounts' credits. This matches priority/weight semantics, but is a separate session-allocation counter, not CPA's per-request accounting counter. OAuth, Codex API-key and compatible-provider candidates are all accepted; CPA remains responsible for model support, cooldown, and excluding already-tried accounts.

## Scheduler priority and authentication

CPA sorts plugin priority **descending**. Priority 5 wins over an effective priority of 0, but loses to a higher-priority Scheduler. Guard declares Scheduler and across-priority candidate support only when session routing is enabled.

Scheduler and frontend authentication are separate capabilities. Key Policy X can continue authenticating and enforcing model access while Guard owns Scheduler. Guard does not require Key Policy-specific `provider` or `key_id` metadata: the actual host execution snapshot carries `caller_scope`, not those authentication fields.

CPA calls only the first eligible Scheduler plugin. `Handled: false` falls back to built-in selection; it does not call Key Policy's Scheduler afterward. Therefore use Key Policy's `routing_mode: host` for this setup. Any legacy `override` group-selection policy needs separate integration before it can coexist with Guard owning Scheduler.

Changing priority alone does not add Scheduler to an older Guard binary. The existing v0.3.8 release predates this implementation. This change is source-only until you build and load a new plugin artifact yourself.

## Sessions, persistence and failure handling

- Caller scope is supplied by CPA after downstream authentication. Guard never reads a client header to establish caller identity. No caller scope means normal host routing.
- Host `canonical_session_id` takes precedence. Other explicit sources include `Session-Id`, `Session_id`, `X-Session-Id`, `X-Session-Affinity`, body `session_id`, `prompt_cache_key`, and conversation ID. All sources use CPA-compatible canonical UUID normalization: remove known protocol wrappers, lowercase UUIDs, and deterministically project other IDs to UUIDv8 with the host's domain separator. The same ID in different source fields produces the same binding. The plugin does not infer identity from prompt text. With no session, the host execution request ID isolates the request.
- Category and selected account are shared across models in the same caller/session. An account that cannot serve a later model causes host fallback; the observed fallback becomes the new session preference. This is soft affinity, not strict account isolation.
- Classification uses the first nonempty effective user text from Responses `input` or Chat Completions `messages`, not the last user turn or any assistant/system/tool content. For newly observed full-history requests, the initial task determines category. For later requests on an active binding, no classification callback runs at all. Tool-only, missing-user, malformed and unsupported multimodal payloads defer to CPA when there is no existing binding.
- Category is fixed until expiration. Routing requests and account selection refresh the sliding TTL. Cache/local-result hits may refresh an existing explicit session's TTL, but never classify, create a binding, select an account or call upstream. Rule/preference edits apply to new/expired bindings; removing a category makes its bindings defer to CPA.
- The versioned state file contains opaque SHA-256 caller/session keys, categories, auth IDs, expirations and pool credits, never prompts or API keys. Use a dedicated file, distinct from Guard/probe state files. Back up the state file with the rest of plugin data if bindings and rotation must survive machine replacement.
- Each transaction takes a stable sibling `.lock` file lock, reloads the current state, removes expired records, writes and syncs a temporary file, then renames it. Linux/macOS use `flock`; Windows uses `LockFileEx`. Expiration, new classification and renewal use one timestamp inside the same transaction, preventing expired categories from being recreated from a stale lookup. Compatible plugin instances coordinate through the file lock. Fallback writes compare the prior account ID so a stale request cannot blindly replace a newer binding.
- Invalid state is not overwritten. Read/write/lock failures defer to CPA and emit a diagnostic when Guard debug logging is enabled. There is a limit of 10,000 active bindings and 1,024 active rotation pools/credits per pool; new allocations defer when full. The small bounded JSON store serializes disk transactions; a different store is warranted only if measured traffic makes this a bottleneck.
- The internal `X-Cpa-Internal-Session-Route` header is replaced on incoming cache-miss requests. Scheduler requires a matching live host request ID and caller scope. After-selection handling removes the header when present, even when routing is disabled; recording fallback does not depend on the header surviving provider rewrites. It emits no header edits when the marker is absent, avoiding unnecessary host session-metadata reconciliation.
- Optional `protected_auths` maps exact auth IDs to allowed categories and a model regex. Non-admitted reserved IDs are removed from every active Guard Scheduler call, including unclassified/fallback requests. If removal makes host deferral unsafe, Guard uses its own priority/weighted fallback over the remaining candidates, or explicitly rejects when none are usable. See the quality-policy document for that exception to unrestricted host fallback and its runtime boundary.

## Verification

Run from the plugin repository:

```sh
go test -race ./...
go vet ./...
```

Regression coverage includes configured classification, first-user extraction, probe context/caller restrictions, concurrent atomic classification, expiry-boundary renewal, canonical-session parity with the host's UUIDv8 golden vector, weighted pool ratios/rotation across instances, zero weights, sticky picks not consuming credits, legacy-state conversion, stream buffer boundaries and routing-only streams, reload persistence, stale fallback protection, sliding expiration, caller isolation, request-only identity, local/cache short-circuit before routing state, unavailable-target deferral, forged markers, disabled caching, invalid reconfiguration and Responses-Lite body repair.

## Transition from the initial routing implementation

This revision fixes all six findings in `session-content-routing-review-2026-11-20.md`.

- A routing pending record does not collect stream bodies unless it also tracks a probe-cache sample. Probe accumulation reserves separator space safely at every buffer length, and releases its mutex with `defer`. Claude snapshot collection remains independent of the probe-cache switch.
- Legacy bare binding maps are read and converted to the versioned `{version, bindings, pools}` file at the next successful transaction. Old routing builds cannot read the new format and will defer without overwriting it. Do not roll back to an old routing build expecting it to retain affinity.
- The initial build hashed source-prefixed session identities (`header:...`, `session:...`) instead of canonical IDs. Because those stored keys cannot be reversed, noncanonical legacy keys are retained until TTL expiry but cannot be automatically associated with the new canonical key. A session first seen by this corrected revision may therefore establish a fresh binding once. Subsequent reloads/restarts of corrected versions retain canonical bindings normally. The initial routing implementation has not been deployed by this work.

Host source paths checked for ABI/order compatibility:

- `sdk/api/handlers/handlers.go`: `requestExecutionMetadata`, `requestCallerScope`.
- `sdk/api/handlers/handlers_interceptors.go`: before-selection propagation into `opts.Headers` and after-selection propagation.
- `internal/pluginhost/adapters_interceptors.go`: clear headers before applying replacements.
- `sdk/cliproxy/auth/conductor_selection.go`: candidate filtering, `schedulerOptions`, `pickViaPluginScheduler`.
- `sdk/cliproxy/auth/conductor_execution.go`: `publishSelectedAuthMetadata`, `applyRequestAfterAuthInterceptor`.
- `internal/pluginhost/snapshot.go`, `scheduler.go`, `adapters_auth.go`: descending priority and independent capability selection.

The plugin tests exercise its JSON ABI and lifecycle; host tests verify host behavior separately. Host `internal/pluginhost` scheduler/priority/frontend-authentication tests and `sdk/api/handlers` interceptor/caller-scope tests passed during implementation. The Windows binding-store test executable also cross-compiles successfully (not executed on Windows). These are not a live end-to-end deployment test of the new Scheduler. No production config or binaries are changed by this implementation.
