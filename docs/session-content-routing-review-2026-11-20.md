# Session content routing review — 2026-11-20

Reviewed commit: `702fbc6` (`git diff a3a63a4...702fbc6`). This is a source review of the new implementation, not a live deployment test. Existing unstaged README and probe-replay changes were excluded from the review scope.

The latest user decisions override the earlier plan: cache/local computation precedes classification; cache hits must not select or call accounts; plugin priority may select Guard's Scheduler independently of Key Policy frontend authentication. No application-source fixes are included in this review.

## Standards / implementation correctness

### P1: routing-only stream records expose a panic and permanently held mutex

`internal/plugin/session_route.go:98–104` creates a pending record even for streams ineligible for probe caching. `internal/plugin/engine.go:612–638` accumulates SSE content into every pending record when probe caching is enabled, without checking whether the record tracks a probe sample.

When accumulated text reaches `maxStreamAccumBytes - 1`, the next chunk computes `room == 1` and executes `chunk[:room-2]`, panicking with a negative slice bound. The panic occurs with `pendingMu` locked; top-level recovery converts it to an error envelope but never unlocks the mutex. Future routing, scheduler, completion and other pending-map operations can block indefinitely.

The stream-buffer boundary defect predates this commit, but this commit broadens exposure to ordinary routed streams, including requests larger than the probe input limit. Routing-only pending records also incur unnecessary response buffering before the boundary is reached.

Reproduced using an isolated test: route a 21,000-character user input, send a chunk of `maxStreamAccumBytes - 3` bytes (the collector adds two separators), then one more byte. Observed `slice bounds out of range [:-1]`; `pendingMu.TryLock()` subsequently fails.

Required direction: separate routing bookkeeping from sample accumulation eligibility, fix the boundary arithmetic, and ensure locks are released on all exits/panics.

### P2: expiration between lookup and renewal resurrects an expired category

`internal/plugin/session_route.go:80–93` looks up a binding and later calls `ClassifyOnce` in a separate transaction. If the binding expires between those operations, `Store.update` deletes it, then `ClassifyOnce` recreates it using the old category passed by the caller. The current request is not reclassified.

Reproduced with a controlled clock: lookup at one nanosecond before expiration, renewal at one nanosecond after expiration. An ordinary request recreates `benchmark` instead of selecting `general`.

Required direction: coordinate the absent/expired decision and classification/creation in the same transaction, or explicitly retry classification when the previously observed binding has expired.

## Spec / requirements coverage

### P2: session identity is not canonical across representations

`internal/sessionroute/identity.go:12–46` prefixes the same session UUID differently depending on its source. Header `Session-Id` yields `session:<uuid>`, `X-Session-Id` yields `header:<uuid>`, and host canonical metadata yields `canonical:<uuid>`. These hash into different bindings for the same caller. Header values also take precedence over the host canonical identity.

Reproduced directly with the same UUID in all three representations. This can split binding continuity when transport/header representation changes. Host `sdk/cliproxy/session/identity.go:66–109` has canonical normalization semantics that the plugin currently does not follow.

### P2: probe rules cannot enforce the planned context safeguards

The plan excludes tools/full-context requests from keyword-only probe classification. `internal/sessionroute/classifier.go:107–133,149–201` examines only model, optional UA and latest user text. It does not expose tool-presence or conversation-complexity conditions to configurable rules.

Reproduced with the configured integer-selection rule, matching UA/model, a tools definition, and a multi-turn real-task context ending in the integer-selection text: classification is `probe`. YAML text regexes cannot distinguish this request from a standalone probe with identical latest user text.

The documentation's example integer rule is broader still: it omits UA/model restrictions. Treat all example rules as illustrative, not verified production templates.

### Semantic discrepancy: first user message versus latest user message

The plan describes first-effective-user-message classification. Current code intentionally scans arrays backward (`classifier.go:175`), and existing tests explicitly expect the final user message to determine category when an earlier user message is SVG-related.

For a newly observed session containing full history, current behavior is **classify the latest user message of the first observed cache-miss request**, not recover the original task from the earliest user message. Existing bindings remain sticky afterward. This semantic choice must be settled explicitly before claiming exact first-user behavior; changing to earliest-user extraction blindly could reintroduce unwanted historical-content matches.

### Selection-policy discrepancy: priority/ID ordering replaces pool balancing

The plan mentions existing weight/availability selection within the eligible probe pool. `internal/sessionroute/store.go:222–250` instead chooses highest priority, then smallest auth ID. New sessions at the same priority concentrate on one account; weights and round-robin are not honored by this custom pick. Host availability/model filtering still applies.

This satisfies the narrower requirement “an available priority < 7 account,” but not weighted/rotating probe-pool selection. Binding to the selected account afterward is intentional.

## Actual behavior flow

1. CPA frontend authentication/model access runs independently of Scheduler ownership. With routing enabled and Guard the highest-priority eligible Scheduler plugin, Guard owns scheduling; Key Policy can retain frontend authentication.
2. Guard validates the request and preserves body repairs. Validation termination returns immediately.
3. If probe caching is enabled, the existing fixed arithmetic handler and then probe-cache lookup run. A successful result returns immediately. Existing explicit session bindings may have their TTL refreshed, but no new classification, account binding or Scheduler call occurs.
4. A cache miss reaches routing only if routing is enabled, model inclusion matches, host caller scope exists and request ID exists. Otherwise CPA continues normally; client internal-marker copies are removed when present.
5. Guard extracts an explicit session identity. If none exists, `request:<host RequestID>` provides single-request isolation. The binding key hashes caller scope and that identity.
6. Existing category is reused; absent category is selected by the first configured rule whose text patterns and optional model/UA conditions match. Text is the latest user text. No text/unsupported payload defers; a valid unmatched text uses `default_category`. Category is persisted before Scheduler reserves an account.
7. A request marker links the live pending record to Scheduler. Scheduler also checks host caller scope. CPA supplies candidates filtered for supported routes/models, availability and previously tried accounts; across-priority capability exposes lower tiers.
8. If a bound auth is present in candidates, it is reused even if it is outside the category's original preferred pool (for example, after fallback). Otherwise an unavailable bound auth defers to CPA. For a new account binding, configured preferred IDs are tried in order; a priority pool filters exclusions/max priority and picks priority then ID.
9. Deferral runs CPA's built-in selection, not the next Scheduler plugin. It does not create new cross-provider candidates absent from the host route.
10. After selection, `selected_auth_id` updates the persisted binding with compare-and-swap against the previously observed account. This records a selected account before upstream success is known. The internal header is removed before execution when present.
11. Completion/response processing handles cache samples and pending cleanup. Ordinary routing pending records currently also reach the unsafe stream accumulation path described above.
12. Bindings persist in a dedicated locked JSON file and use sliding TTL. Cache hits renew existing explicit bindings; misses/account selections also renew activity. State failures defer to CPA. HTTP/WS/retry hooks were inspected statically, but this review does not establish that every WS continuation reschedules an already selected connection.

## Verification evidence

Four isolated review tests were supplied through Go's `-overlay` facility using temporary files under `/tmp/opencode/`, without editing the plugin source:

```sh
go test -overlay /tmp/opencode/cpa-session-route-review-overlay.json \
  ./internal/plugin -run '^TestReview' -count=1 -v -timeout=20s
```

All four intentionally asserted the desired behavior and failed, confirming:

- stream panic plus leaked mutex;
- expired-category resurrection;
- divergent equivalent session representations;
- tool-bearing multi-turn context entering the probe pool.

The earlier passing race/unit tests did not cover these cases. No server build, restart, deployment, production-config change or direct database modification was performed.

## Summary

Implementation axis: two confirmed defects; worst is the stream panic/mutex leak. Spec axis: two reproduced coverage defects and two documented semantic/policy discrepancies. Fix the confirmed defects and settle the semantic/policy choices before deploying this feature.
