# Proposed thinking route

Apply only after installing a Guard build that permits an empty default route;
the currently deployed version rejects this configuration. Merge these changes
into the complete live Guard configuration, preserving other fields and rules.

* Set `categories.general` to `{}`. It has no preferred account; existing
  bindings can remain sticky, and protected-account filtering can still affect
  fallback selection.
* Add `categories.thinking.preferred_auth_ids` containing the existing general
  API-key auth ID. Add `thinking` to that auth's `protected_auths` categories,
  retaining `general` so it remains eligible for native fallback.
* After the existing quality rules, add two `thinking` rules with
  `model_include_regex: '^gpt-'`, `require_reasoning_effort: true`,
  and respectively `min_user_chars: 1000` and `min_context_chars: 16000`.
  Existing quality rules precede them. Any nonempty effort is accepted.
* Change the existing `say hi` probe rule's `user_agent_regex` to `.*`, retaining
  its exact text, standalone, no-tools and input-size constraints.

An empty default route is accepted by the proposed source revision, but it does
not guarantee unrestricted native selection: the scheduler filters protected
credentials and may use its protected fallback. Existing session bindings and
cache hits retain their usual behavior and do not immediately reclassify.
