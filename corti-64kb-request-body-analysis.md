% Corti — 64 KiB Request-Body Clip on the Public LLM Gateway
% Status: analysis / handoff (not yet a code change)
% Date: 2026-08-07

# Purpose

This document captures the analysis of a hard limit that truncates HTTP **request bodies
larger than 64 KiB** on the Corti **public LLM gateway**. It is a handoff for another
LLM/engineer to pick up and implement a fix. Every claim below was verified against
source at the time of writing. It is intentionally specific: file paths, line numbers,
function names, and exact edits are given so the fixer can act without re-deriving the
system.

---

## TL;DR

- The 64 KiB clip is **not** a chart value, Envoy Gateway policy override, or Envoy
  network-buffer setting. It is the **default `max_message_size` (65536 bytes) of the
  `envoy.filters.http.ext_proc` filter** that the **ai-gateway-controller injects**.
- That filter is injected with **`RequestBodyMode = BUFFERED` / `ResponseBodyMode = BUFFERED`**
  (half-duplex), which requires Envoy to buffer the whole body in memory up to
  `max_message_size`; bodies past that error out.
- The filter config is **hardcoded in the controller source**, not exposed via
  `GatewayConfig`, chart values, or `EnvoyPatchPolicy`. It cannot be changed from the
  `charts` repo or the `deployments` repo as-is.
- **There is no supported way to enable full-duplex today.** It is an open upstream
  issue and neither side (filter injection nor the extproc server) implements it.
- The only real fix at present: **patch the controller source** to raise
  `max_message_size` on the injected ext_proc filter, build a `-corti` controller image,
  and pin it. This doc lives in the `corticph/ai-gateway` fork for exactly that.

---

## Context: how the public LLM gateway is assembled

Three repos are involved:

1. **`corticph/charts`** — the `envoy-gateway` chart
   (`charts/envoy-gateway/`, version `3.17.0`, appVersion `v1.7.1`) renders the
   `publicLLMGateway` object: a `Gateway`, `GatewayConfig`, `EnvoyProxy`, SecurityPolicy,
   and Lua-based `EnvoyExtensionPolicy` (prompt-injection, billing, model-rewrite). It also
   bundles the `ai-gateway-helm` chart as a dependency.
2. **`corticph/deployments`** — deployment values.
   `config/services/envoy-gateway/base/values.yaml` pins a **Corti-patched extproc image**:
   `corti.azurecr.io/platform/ai-gateway-extproc:v1.0.0-corti.1`.
3. **`corticph/ai-gateway`** — this fork of `envoyproxy/ai-gateway`. Contains the
   ai-gateway-**controller** (Extension Server) and **extproc** (External Processor).

Data plane flow for a request:

```
client
  └─(HTTPS)-> envoy-gateway proxy (the publicLLMGateway LB)
       └─ [Lua filters: prompt-injection, billing, model-name-rewrite]
       └─ [envoy.filters.http.ext_proc/aigateway]  <- the 64KiB clip lives here (half-duplex BUFFERED)
            └─(UDS gRPC)-> ai-gateway-extproc sidecar
                 └─ routes via AIGatewayRoute, does prompt/auth, token-count metadata
       └─-> upstream LLM provider
```

The ext_proc filter must see the **entire request body** because it parses the JSON to
determine the target model/provider and publish token-usage metadata (which billing.lua
reads). In `BUFFERED` mode Envoy holds the whole body and sends it in one gRPC message,
bounded by `max_message_size`.

---

## Root cause (exact location)

The injected ext_proc filter is built in the **controller**, file
`internal/extensionserver/post_translate_modify.go`, function
`insertRouterLevelAIGatewayExtProc` (around lines **760–828**):

```go
// internal/extensionserver/post_translate_modify.go
epAny, err := toAny(&extprocv3.ExternalProcessor{
    GrpcService: ...,
    MetadataOptions: ...,
    ProcessingMode: &extprocv3.ProcessingMode{
        RequestHeaderMode:   extprocv3.ProcessingMode_SEND,
        RequestBodyMode:     extprocv3.ProcessingMode_BUFFERED,   // <-- half-duplex buffering
        RequestTrailerMode:  extprocv3.ProcessingMode_SKIP,
        ResponseHeaderMode:  extprocv3.ProcessingMode_SEND,
        ResponseBodyMode:    extprocv3.ProcessingMode_BUFFERED,   // <-- half-duplex buffering
        ResponseTrailerMode: extprocv3.ProcessingMode_SKIP,
    },
    MessageTimeout:    durationpb.New(10 * time.Second),
    FailureModeAllow:  false,
    AllowModeOverride: true,
})
```

The filter is registered by name `aiGatewayExtProcName = "envoy.filters.http.ext_proc/aigateway"`
(const at line **43**), and inserted into the Envoy listener's HTTP connection manager via
`insertAIGatewayExtProcFilter` (line ~813) / `shouldAIGatewayExtProcBeInserted` (line ~1003).

There is a **second, different** ext_proc filter in the same file (the *upstream* filter built
in `maybeConfigureBackend`/`modifyClusterForAIGateway`, around lines **432–470**) which uses
`RequestBodyMode: NONE` / `ResponseBodyMode: NONE` and is **not** the source of this clip.

### Why the 64 KiB number

`ExternalProcessor` does **not** set `max_message_size`, so Envoy applies its default of
**65536 bytes (64 KiB)**. Envoy docs for `BUFFERED` body mode:

> "Buffer the message body in memory and send the entire body at once. If the body exceeds
>  the configured buffer limit, then the downstream system will receive an error."

That is exactly the observed clip. To raise it, set `MaxMessageSize` on this struct.

---

## Why it can't be fixed from the charts/deployments

Verified, so nobody re-tries these dead ends:

### 1. `EnvoyPatchPolicy` (in the `charts` repo) cannot target the filter

- EG v1.7.1's `EnvoyPatchPolicy` JSON patches only support **top-level xDS resource types**:
  `Listener`, `Route`, `Cluster`, `Endpoint`, `Secret`. There is **no `HTTP_FILTER` type**
  (`internal/xds/translator/jsonpatch.go`, `getXdsResourceType`/`findXdsResource`).
- Even a `Listener` patch runs **before** the extension hook: in
  `internal/xds/translator/translator.go`, `processJSONPatches` (line **157**) executes *
  before* `processExtensionPostTranslationHook` (line **165**). The ai-gateway-controller
  injects the ext_proc filter inside its `PostTranslateModify`, which is the extension hook —
  so the filter **does not exist yet** when EG applies patches.
- The controller rebuilds the filter from scratch on every reconcile, so any manual
  listener edit would be clobbered.

### 2. `GatewayConfig` exposes no knob

`api/v1beta1/gateway_config.go` — `GatewayConfigSpec` only has `extProc` (container
resources/env), `globalLLMRequestCosts`, and `forwardProxy`. No `maxMessageSize` /
message-size / body-size field.

### 3. Network buffer settings do not help

Raising `clientTrafficPolicy.bufferLimit` (chart default 50 Mi; production overrides to
**200 Mi** in `deployments/config/services/envoy-gateway/production/values.yaml`) only
raises the downstream **connection/network buffer**. It does **not** change the ext_proc
`max_message_size`. Coding-agent bodies (5–80 MiB) still fail at 64 KiB even with
`bufferLimit: 200Mi`.

---

## Full-duplex is NOT available today

Full-duplex would stream the body and avoid the buffering cap entirely, but it is not
implemented:

- **Controller / filter side:** no `FULL_DUPLEX` / `FullDuplexStreamed` anywhere in
  `post_translate_modify.go` (still `BUFFERED` on current `main`).
- **extproc server side:** `internal/extproc/` has **zero** `FULL_DUPLEX` / `StreamedBodyResponse`
  support, so the sidecar cannot speak the full-duplex protocol.
- **Upstream status:** open issue
  [envoyproxy/ai-gateway#527](https://github.com/envoyproxy/ai-gateway/issues/527)
  "Use ext_proc full duplex mode to avoid Envoy's limits on body size" — still **open + stale**.
  A maintainer noted the migration could start once Envoy Gateway shipped duplex
  (EG PR #5349), but it was never done. Related upstream issue #1662
  (`response_payload_too_large`).

### Full-duplex requirements (for the future migration)

Per Envoy ext_proc docs, switching to `FULL_DUPLEX_STREAMED` requires:
- `RequestTrailerMode`/`ResponseTrailerMode` set to `SEND` (trailers signal end-of-stream).
- The external processor must respond with `StreamedBodyResponse` (not `BodyResponse`).
- The extproc server would have to be rewritten to do streaming parse (its current JSON
  parse is all-or-nothing on a buffered body).

This is a substantial change on **both** sides; it is not a one-line flip.

---

## The realistic fix (near-term): raise `max_message_size` on the injected filter

Minimal, keeps buffered (half-duplex) mode. Requires a **controller** source change + a
patched controller image; the extproc image is unchanged.

### 1. Edit `internal/extensionserver/post_translate_modify.go`

In `insertRouterLevelAIGatewayExtProc`, the `ExternalProcessor` literal built around
line **777**, add:

```go
epAny, err := toAny(&extprocv3.ExternalProcessor{
    GrpcService: ...,
    MetadataOptions: ...,
    Memcached: ... ,      // (existing fields unchanged)
    MaxMessageSize: wrapperspb.UInt32(<desired bytes>),   // <-- ADD
    ProcessingMode: &extprocv3.ProcessingMode{ ... },      // unchanged (stay BUFFERED)
})
```

`MaxMessageSize` is a `*wrapperspb.UInt32Value` (from
`google.golang.org/protobuf/types/known/wrapperspb`). Example values:
- 200 Mi: `wrapperspb.UInt32(200 * 1024 * 1024)` → `209715200`
- 80 Mi:  `wrapperspb.UInt32(80 * 1024 * 1024)` → `83886080`

Make the size **configurable** (preferred, so deployments can tune per environment):

- Add a controller flag in `cmd/controller/main.go` (e.g. `--extProcMaxMessageSize`,
  parsed with the other `fs.StringVar/IntVar` flags) and thread it through the Server
  struct (`internal/extensionserver/server.go`) into `post_translate_modify.go`.
- OR read from an env var (the extproc side already reads env for sizing; mirror that
  pattern). If you add a flag, also expose it via the chart's `ai-gateway-helm` values
  (`controller.extraArgs` / flags) — the `ai-gateway-helm` subchart already supports
  `controller.extraEnvVars`; check for an args mechanism before adding one.

Also consider raising the **extproc-side gRPC receive limit**: `cmd/extproc/mainlib/main.go`
already defines a `maxRecvMsgSize` flag (a UDS gRPC receive cap, separate from Envoy's
`max_message_size`). Make sure the controller-emitted message never exceeds it, or raise it
to match.

### 2. Build a `-corti` controller image

The existing patch convention is a `-corti.<n>` suffix on a fork build. The extproc image
is already `v1.0.0-corti.1`; the controller image is still stock. Build the controller as
`corti.azurecr.io/platform/ai-gateway-controller:v1.0.0-corti.2` (next suffix after the
content-length patch) or a fresh branch.

### 3. Pin it in `corticph/deployments`

`config/services/envoy-gateway/base/values.yaml`, under `ai-gateway-helm`:

```yaml
ai-gateway-helm:
  extProc:
    image:
      repository: corti.azurecr.io/platform/ai-gateway-extproc
      tag: v1.0.0-corti.1
  controller:
    image:                      # <-- ADD (currently only mutatingWebhook is set)
      repository: corti.azurecr.io/platform/ai-gateway-controller
      tag: v1.0.0-corti.2
```

Then tune the max-message value per environment in
`config/services/envoy-gateway/{development,staging,production}/values.yaml` (production
already targets 200 Mi coding bodies).

### 4. Optional chart plumbing (if you add a flag)

In `corticph/charts/charts/envoy-gateway`, if you expose the flag through the
`ai-gateway-helm` dependency, you can add a `controller` values passthrough. This is
optional and only meaningful if step 1 makes the size configurable.

---

## Verification checklist (after deploying the patched controller)

- [ ] `kubectl -n shared get envoypatchpolicies` — confirm no accidental patches created.
- [ ] Inspect the live Envoy listener xDS: the injected filter
      `envoy.filters.http.ext_proc/aigateway` should show
      `"max_message_size": 209715200` (or your chosen value) and
      `processing_mode.request_body_mode: BUFFERED`.
      (Check via `egctl xds translate` from the Envoy Gateway controller, or a proxy
      config-dump.)
- [ ] Send a request with a body > 64 KiB (e.g. 10 MiB) to the public LLM gateway and
      confirm it is not truncated (previously fails).
- [ ] Confirm production `clientTrafficPolicy.bufferLimit: 200Mi` is still in place so the
      downstream connection buffer does not become the next bottleneck.
- [ ] Watch extproc memory: buffered large bodies live in Envoy + extproc memory; confirm
      the extproc container's memory limit (production: 1 Gi) is sufficient for the largest
      sanctioned request, else raise it via `ai-gateway-helm.extProc.resources`.

---

## Quick reference — key locations

| Concern | File | Symbol / line |
|---|---|---|
| Injected filter (BUFFERED + missing MaxMessageSize) | `internal/extensionserver/post_translate_modify.go` | `insertRouterLevelAIGatewayExtProc` ~L760–828; struct L777 |
| Filter name | `internal/extensionserver/post_translate_modify.go` | `aiGatewayExtProcName` L43 |
| Upstream (non-buffered) filter — NOT the clip | `internal/extensionserver/post_translate_modify.go` | `maybeModifyCluster*` L432–470 |
| Controller flags | `cmd/controller/main.go` | `fs.*Var` ~L112 |
| extproc gRPC recv cap | `cmd/extproc/mainlib/main.go` | `maxRecvMsgSize` flag L147 |
| GatewayConfig API (no knob) | `api/v1beta1/gateway_config.go` | `GatewayConfigSpec` |
| EG patch ordering (@ upstream) | `envoyproxy/gateway v1.7.1` `internal/xds/translator/translator.go` | `processJSONPatches` L157 before extension hook L165 |
| Extreme gating for future full-duplex | `internal/extproc/` | no FULL_DUPLEX anywhere |

---

## Open decisions for the implementer

1. Hardcode a single size vs. make it configurable (flag/env var). Recommend configurable.
2. Which size? Production comment targets **200 Mi** for coding-agent bodies; pick a default
   (e.g. 80 Mi) with per-env override.
3. Whether to also pursue the upstream full-duplex migration (larger effort, both sides) or
   ship the `max_message_size` patch first (small, unblocks today). Recommend the patch now,
   track full-duplex upstream.
