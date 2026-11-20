# Reserved quality-account routing — 2026-11-20

## Decision

Keep cache/local computation first and retain existing special-task routing. Split ordinary requests into two categories:

| Traffic | Preferred destination |
| --- | --- |
| Local result or cache hit | No account selection or upstream call |
| Confirmed SVG-generation / candy-task templates | Existing `guilioenricomeille` OAuth |
| Confirmed standalone probes, including the integer-selection template | Existing priority `< 7` pool, excluding all reserved accounts |
| Ordinary astra/sol requests with explicit medium-or-higher effort and substantive task/context | `cwhyptucsd@gmail.com` Codex OAuth, currently priority 4 |
| Other ordinary requests | Existing Codex API-key account ending `...pI` |

New quality admission requires:

1. Model matches `^gpt-[0-9]+(?:\.[0-9]+)*-(?:astra|sol)$`.
2. Explicit effort is one of `medium`, `high`, `xhigh`, `max` (the list is configurable, not an implicit ranking in Go).
3. First effective user text has at least **1,000 Unicode characters**, **or** visible conversation context has at least **16,000 Unicode characters**.

Use two ordered rules to express the OR in point 3. Do not infer effort from an absent field. The classifier reads Responses `reasoning.effort` or Chat `reasoning_effort`; a trusted host `reasoning_effort` metadata value may supplement an absent body value. An explicit body value takes precedence.

Once admitted, the existing caller/session binding remains sticky: short follow-ups and omitted/reduced effort do not force an account switch. The reserved-account model allowlist still applies on every Scheduler call, so a switch to a different model family cannot borrow the account. Existing ordinary bindings are not automatically promoted; rule changes apply to unbound/expired sessions.

The account file `codex-fb9054d7-cwhyptucsd@gmail.com-plus.json` was inspected read-only: provider type `codex`, priority `4`, not disabled. This is an on-disk observation, not proof that the running instance offers it as an eligible candidate. Use CPA's exact Scheduler auth ID in configuration, not the email or raw OAuth token.

## Evidence and threshold choice

Reproducible local analysis:

```sh
python3 scripts/analyze-routing-lengths.py --self-test
python3 scripts/analyze-routing-lengths.py /home/cwhypt/.cli-proxy-api/logs
```

Final recorded scan: **1,017 matching files**, **1,010 parsed GPT requests**, seven non-GPT records excluded. Downstream request timestamps ranged from **2026-11-20 12:35:14 to 15:12:05, UTC−08:00**. Only local currently retained `/v1/responses` and `/v1/chat/completions` requests were included, including error-log counterparts when present. The scan does not establish the distribution outside this retention window. Logs rotated during the investigation, so reruns have different ranges/counts; the reader reports changed/disappeared/unreadable files separately.

Of astra/sol requests, **587** omitted effort, **193** declared `medium`, and **10** declared `max`. This window contained no explicit `high`/`xhigh` samples. Missing effort is not counted as medium by assumption. These are request observations, not unique tasks, users or successful upstream completions.

For the **203** explicit medium-or-higher samples, deterministic 1-D k-means (`k=3`, `log(1 + context_chars)`, evenly spaced initial centers, up to 50 iterations) produced:

| Group | Requests | Visible context characters | Interpretation limit |
| --- | ---: | ---: | --- |
| Small repeated task | 10 | 306 | Includes the existing candy-task template; special routing takes precedence |
| Initial tasks / short contexts | 16 | 1,308–9,947 | Mixed initial review tasks and SVG tasks, not one semantic quality class |
| Developed contexts | 177 | 37,429–680,066 | Mostly repeated snapshots of a small number of long-running coding/review sessions |

A recurring real coding session had a **22-character first user message**, total user text only hundreds of characters, but **63,036–680,066** characters of actual conversation/tool-result context. Counting only user messages, or taking an arbitrary 4,000-character user threshold, would miss that workload. Conversely, counting the JSON envelope would admit requests inflated only by system instructions or tool schemas.

The **16,000-character context gate** lies in the observed gap between 9,947 and 37,429. The **1,000-character first-user gate** also admits substantive initial review tasks (observed at 1,308 and 1,406 characters), before their conversation history grows. Known greetings were around 2–6 characters, the fixed arithmetic template around 113–115, and integer-selection variants around 241–265. Existing candy/SVG templates can exceed the new gates, which is why their exact-template rules must remain earlier.

These cutoffs are operational admission heuristics, not semantic proof of quality. This sample is heavily influenced by this project's own coding/review session; repeated snapshots are not independent evidence. In particular, simply mentioning SVG/candy inside a code-review prompt must not classify it as the corresponding task—use anchored confirmed task templates, not broad keyword mentions.

## Length measurement

`min_user_chars` counts the first effective user message's text after trimming surrounding whitespace. It does not count JSON escaping, system/developer instructions or schemas. `min_context_chars` counts visible user/assistant/tool/function message text and textual `function_call_output` results. It excludes structured images/base64 blocks, encrypted reasoning, function-call arguments, tool definitions, and system/developer messages/instructions. Plain text tool output still counts as actual context; length alone cannot distinguish useful output from irrelevant output.

No conversation bodies or API keys are emitted by the analyzer or written into this report. Repeated-template fingerprints normalize whitespace and numbers and are only statistical grouping hints, not production classification rules.

## Configuration fragment

Merge into Guard's existing `session_routing` mapping. Keep existing rules before the two new quality rules; keep the ordinary default category and both existing preferred-account IDs unchanged.

```yaml
categories:
  # Keep general and benchmark unchanged.
  quality:
    preferred_auth_ids: ['<cwhyptucsd OAuth auth ID>']
  probe:
    max_priority: 6
    exclude_auth_ids:
      - '<ordinary Codex API-key auth ID>'
      - '<guilioenricomeille OAuth auth ID>'
      - '<cwhyptucsd OAuth auth ID>'

protected_auths:
  '<cwhyptucsd OAuth auth ID>':
    categories: [quality]
    model_include_regex: '^gpt-[0-9]+(?:\.[0-9]+)*-(?:astra|sol)$'

rules:
  # Keep exact SVG/candy and confirmed-probe rules here, in that order.
  - category: quality
    model_include_regex: '^gpt-[0-9]+(?:\.[0-9]+)*-(?:astra|sol)$'
    reasoning_efforts: [medium, high, xhigh, max]
    min_user_chars: 1000
  - category: quality
    model_include_regex: '^gpt-[0-9]+(?:\.[0-9]+)*-(?:astra|sol)$'
    reasoning_efforts: [medium, high, xhigh, max]
    min_context_chars: 16000
```

Account IDs, model patterns, efforts, and thresholds are YAML/management-config data; they are not compiled account/task rules in Go.

## Reserved-account protection and fallback

Priority 4 alone is not protection: it places the account inside the old `< 7` probe range. Explicit probe exclusion is supplemented by `protected_auths`, which filters Scheduler candidates unless a trusted live session category and the requested model admit that auth ID. Forged/missing markers and unclassified requests cannot authorize it.

- A matching preferred/bound **allowed** account is selected as before.
- If no protected candidate was removed, ordinary unhandled decisions still defer to CPA's original selector.
- If a protected candidate was removed and the preferred/bound account is unavailable, delegating the unfiltered list to CPA would let the account leak back in. Guard therefore chooses a fallback from **remaining unreserved candidates**, using highest available priority and persistent smooth weighted rotation, and updates the existing binding atomically.
- If no allowed candidate remains, or protected fallback state cannot be safely updated, Guard rejects with `auth_not_found` rather than borrowing the reserved account. This intentionally narrows the earlier unrestricted-fallback policy for this protected account.
- This filtered fallback uses Guard's own pool accounting, not CPA's existing per-request weighted/session-affinity bookkeeping. Preferred mappings and ordinary safe host deferral are preserved; the exceptional protected-fallback path is now explicitly constrained.

Protection operates while this configured Guard Scheduler is active. Disabling routing, using an older/non-owning Scheduler, or the host fusing the plugin returns control to host behavior; a plugin cannot enforce a global account restriction after its hooks have been bypassed. A host-wide fail-closed admission boundary would require a separate host change. No such host change is included here.

## Verification and deployment status

Regression tests cover model/effort/Unicode-length gates, medium metadata precedence, envelope inflation, long tool-result context, special-rule precedence, short sticky follow-ups, protected fallback, exhausted safe pools, forged markers, unrelated models, and unchanged host deferral when the reserved auth is absent.

This work changes source/tests/docs only. Production `config.yaml`, auth files, running plugin binaries, services, and databases are not modified. Session routing was not configured in the inspected production config; changing Guard's plugin priority to 5 alone does not activate these rules.
