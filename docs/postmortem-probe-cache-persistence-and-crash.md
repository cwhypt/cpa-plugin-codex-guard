# Postmortem: Probe Cache Silent Persistence Failure & `strings.Builder` Host Crash

> Date: 2026-09-06
> Plugin: `cpa-codex-guard` v0.3.0 (Windows / CLIProxyAPI)
> Impact: probe cache samples lost across restarts; CLIProxyAPI process crash

This document records two production incidents discovered while activating the
streaming probe-cache feature, their root causes, and the exact commands
(incorrect vs. correct) so the mistake is never repeated.

---

## Incident 1: `strings.Builder` copied by value crashes the whole CPA process

### Symptom

Every streaming (`stream: true`) request died mid-stream: the client received
`ConnectionResetError`, the request never reached `request.complete`, and the
CLIProxyAPI process **terminated**.

Debug log stopped right after `stream-init` with no `complete` line:

```text
[stream-init] reqId=... eligible=true hash=...
(nothing further — process dead)
```

### Root Cause

The pending-entry map stored `pendingInfo` **by value**, and `pendingInfo`
embeds a `strings.Builder`:

```go
type Engine struct {
    pending map[string]pendingInfo // WRONG: value type
}

func (e *Engine) handleStreamChunk(...) {
    e.pendingMu.Lock()
    p, ok := e.pending[req.RequestID] // struct copy, incl. strings.Builder
    if ok {
        p.streamText.Write(req.Body)  // PANIC: illegal use of copied Builder
        e.pending[req.RequestID] = p
    }
    e.pendingMu.Unlock()
}
```

`strings.Builder` forbids copying after its first write (internal
`copyCheck`). Fetching the struct from a map **always copies it**, so the very
first payload chunk panicked:

```text
panic: strings: illegal use of non-zero Builder copied by value

goroutine 17 [running, locked to thread]:
strings.(*Builder).copyCheck(...)
strings.(*Builder).Write(...)
cpa-codex-guard/internal/plugin.(*Engine).handleStreamChunk(...)
main.cliproxyPluginCall(...)
```

Because the plugin runs **in-process** (c-shared DLL), the panic killed the
host — the entire CLIProxyAPI gateway.

### Fix

Store pending entries **by pointer** so the Builder is never copied:

```go
type Engine struct {
    pending map[string]*pendingInfo // pointer type
}

func (e *Engine) handleStreamChunk(...) {
    e.pendingMu.Lock()
    p, ok := e.pending[req.RequestID] // p is *pendingInfo — no copy
    if ok {
        p.streamText.Write(req.Body)  // safe
    }
    e.pendingMu.Unlock()
}
```

Fix commit: `d863e2b fix: store pending entries by pointer to stop Builder-copy host crash`

### Lesson

Never embed `strings.Builder`, `sync.Mutex`, or any other non-copyable type in
a struct that is stored **by value** in a map — especially in-process plugins,
where a plugin panic takes down the whole gateway.

---

## Incident 2: Silent persistence failure caused by a wrong working directory

### Symptom

The probe-cache state file stopped updating at `10:36`. Every sample collected
afterwards (verified in the debug log with `sample hash=... textOK=true`)
**silently vanished** on the next process restart, and the half-hourly health
probe never accumulated the 3 samples required for cache activation.

The store persistence error was invisible because the call swallowed it:

```go
_ = s.saveLocked()
```

### Root Cause

When restarting CPA, `Start-Process` was invoked **without**
`-WorkingDirectory`. The process working directory fell back to the shell's
CWD (`C:\Users\cwhyp\Documents\GitHub` instead of
`C:\Users\cwhyp\Documents\GitHub\CLIProxyAPI`).

The plugin resolves its relative state paths against the **process working
directory**:

```go
// config defaults / config.yaml
state_file:      "data/cpa-codex-guard-state.json"
probe_state_file:"data/cpa-probe-cache-state.json"
```

With the wrong CWD every write landed outside the real data directory (or
failed outright), and because the error was discarded, nothing in the logs
revealed the problem. Only after adding explicit error reporting
(`LastError()` + `PERSIST_ERROR` debug lines) did the failure surface.

### Incorrect command (missing `-WorkingDirectory`)

```powershell
# WRONG — plugin resolves data/ relative to the shell CWD
Start-Process -FilePath "C:\Users\cwhyp\Documents\GitHub\CLIProxyAPI\CLIProxyAPI.exe" `
    -ArgumentList "--config","config.yaml" `
    -WindowStyle Hidden
```

### Correct command

```powershell
Start-Process -FilePath "C:\Users\cwhyp\Documents\GitHub\CLIProxyAPI\CLIProxyAPI.exe" `
    -ArgumentList "--config","config.yaml" `
    -WorkingDirectory "C:\Users\cwhyp\Documents\GitHub\CLIProxyAPI" `
    -WindowStyle Hidden
```

Alternative (equivalent, sets CWD before launch):

```powershell
Set-Location "C:\Users\cwhyp\Documents\GitHub\CLIProxyAPI"
.\CLIProxyAPI.exe --config config.yaml
```

### Fix

1. Always pass `-WorkingDirectory` when launching CPA.
2. Persist errors are no longer swallowed — `Store.LastError()` records the
   last write failure and the engine logs it:

```text
[collect] PERSIST_ERROR reqId=... store=probe err=<real OS error>
[stream-collect] PERSIST_ERROR reqId=... store=probe err=<real OS error>
```

### Lesson

1. Relative state paths are resolved against the **process CWD**; when
   launching a daemon with `Start-Process`, always pass `-WorkingDirectory`.
2. Never discard errors from atomic persistence helpers (`_ = save()`).
   Surface them (log or metric) — a silent write failure only shows up as
   mystery data loss later.

---

## Verification Performed After the Fixes

1. **Crash**: 4 consecutive streaming requests completed normally; the process
   survived (previously the 3rd or 4th request killed it).
2. **Stream sample collection**: 3 samples collected from live SSE traffic
   (`textOK=true`).
3. **Cache hit**: the 5th identical request was served from the probe cache in
   **56 ms** with a synthesized Responses SSE replay (previously 8–10 s upstream
   calls every 30 minutes).
4. **Persistence**: two consecutive samples (13:38, 13:40) were written to
   `data/cpa-probe-cache-state.json` immediately, with no `PERSIST_ERROR`.

---

## Diagnostic Tooling Added Along the Way

- `data/cpa-codex-guard-debug.log` — plugin-side lifecycle log covering
  `before` / `collect` / `stream-init` / `stream-collect` / `complete` stages,
  including empty-text raw payload dumps and persistence errors.
- `Store.LastError()` on both the guard state store and the probe cache store.
