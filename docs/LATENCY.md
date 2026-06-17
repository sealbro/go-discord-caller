# End-to-End Audio Latency

## Full Audio Path (e.g. `RaidModeGuildCaller` / many-callers mode)

```
User mic
  │  +20ms  Opus frame accumulation (Discord client)
  ▼
[Discord voice server]
  │  +65ms  UDP network (inbound)
  ▼
[VoiceReceiver]  decode + fanout inline (on disgo's UDP goroutine)
  │  +0.3ms allowUser lookup, Opus decode, dispatch to SourceBuffer.Feed (gdc_opus_receive_duration ~0.3ms avg)
  ▼
[SourceBuffer ring]  cap 3 frames; gdc.mixer.pipeline.latency (Prom: gdc_mixer_pipeline_latency_milliseconds)
  │  +37ms  Pull on next 20ms tick; overflow drops oldest at producer (measured: p50 37ms, p95 63ms)
  ▼
[Mixer.tick()]  mix-minus, encode (0ms passthrough if single source)
  │  +2ms   gdc_mixer_tick_duration p95 ~2ms
  ▼
[VoiceProvider]  → UDP send
  │  +0ms   provider channel queue ≈0 (passthrough) + disgo sender
  ▼
[Discord voice server]
  │  +65ms  UDP network (outbound)
  ▼
[Listener's Discord client]  Opus decode → speakers

Total: ~200ms (single guild, many-callers/mix mode; ~185ms in OneCaller copy mode)
```

---

## Latency Budget

| Hop                                       | Typical    | Notes                                                                                                    |
|-------------------------------------------|------------|----------------------------------------------------------------------------------------------------------|
| User mic → Discord voice server           | 20–50ms    | 20ms fixed frame accumulation + network                                                                  |
| Discord voice server → `VoiceReceiver`    | 20–55ms    | Voice UDP leg, same region as voice server                                                               |
| `allowUser` + Opus decode + fanout `Feed` | ~0.3ms     | Inline on disgo UDP goroutine, no channel hop; measured `gdc_opus_receive_duration` ~0.3ms avg          |
| `SourceBuffer` ring → `Mixer.tick`        | **37ms (p95 63ms)** | `gdc.mixer.pipeline.latency` (Prom: `gdc_mixer_pipeline_latency_milliseconds`); ceiling = `audioSourceCap`×20ms = 60ms; measured p50 37ms / p95 63ms; producer-side overflow keeps near live edge |
| Mixer tick processing + encode            | ~2ms       | `gdc_mixer_tick_duration` p95 ~2ms; 0ms on single-source passthrough                                     |
| `VoiceProvider` channel queue             | ~0ms       | `gdc_opus_provide_duration` ≈0 (passthrough); drain threshold 3 frames = 60ms cap                       |
| `VoiceProvider` → Discord voice server    | 20–55ms    | |
| Discord voice server → listener client    | 20–50ms    | |
| **Total (single guild)**                  | **~200ms** | Discord network dominates; in-process pipeline measures ~40ms                                            |

> Measured 2026-06-17 from `grafanacloud-sealbro-prom` (30d aggregate; histograms only populate during live sessions). In-process stages run faster than the original conservative estimates — the ~60ms `SourceBuffer` + tick stage is the only meaningful in-process cost.

**Inter-guild relay** (`ally.Session` → relay mixer → guest `VoiceProvider`) adds ~60ms on top (extra mixer tick alignment + guest guild network leg) → **~260ms total**.

---

## Important Design Notes

### `RaidModeOneCaller` auto-switches between copy and mix mode
`RaidModeOneCaller` no longer statically bypasses the pipeline. As of the auto-switch pipeline (commit #37), `OneCallerPipeline` (`internal/manager/pipeline/one_caller.go`) builds an **always-on mixer graph** — per-speaker-channel mixers plus the relay mixer, created up-front and started paused — and a `router` (`internal/manager/router`) flips the owner source between two modes based on the live caller count in the owner channel:

- **Copy mode** (single caller): the owner's `FanoutHandle` is installed with raw `OpusTargets` per speaker `chOut` plus an `OpusCallback` that calls `ally.Session.BroadcastFromGuild` — all inline on disgo's UDP goroutine. No decode, no mix, no re-encode, no fanout goroutine. Mixers stay paused. **End-to-end ≈185ms typical** — the fast path.
- **Mix mode** (multiple callers): the source decodes into the per-channel and relay mixers via `SourceBuffer`, picking up the full mixer budget above (~200ms).

The `FanoutHandle` is wired by `routerInstallBuilder` (`internal/manager/pipeline/install.go`), not the old `installFanoutDirect` — that function no longer exists. `router.Recompute` decides the mode at session start (`start` closure) and on every caller join/leave, so the latency profile of a OneCaller session is dynamic, not fixed. The same copy/mix split applies to the `RaidModeOneManyGuildCaller` / `StarCaller` topologies.

### The mixer tick is the biggest fixed cost
`Mixer.Run` parks on a 20ms timer (`mixerFrameDur` in `internal/opus/mixer.go`). A frame arriving just after a tick must wait up to 20ms for the next one. This is irreducible without changing Discord's frame cadence.

### Producer-side overflow (no burst drains)
`SourceBuffer.Feed` drops the **oldest** frame on overflow (`internal/opus/source.go:51`). The previous pull-from-channel design needed an explicit drain-threshold step inside `Mixer.tick` that could discard several frames in one go (audible burst gap). Now the consumer always sees a buffer ≤ 60ms; latency cannot accumulate past that ceiling.

| Drain point                          | Mechanism                                | Max tolerated backlog |
|--------------------------------------|------------------------------------------|-----------------------|
| `SourceBuffer.Feed` (producer)       | drop oldest on full ring                 | 60ms (3 frames)       |
| `VoiceProvider.ProvideOpusFrame`     | `providerDrainThreshold` = 3 frames      | 60ms                  |

### Mixer auto-pause via `DrainWatcher`
Channel mixers pause after `DrainIdleTimeout` (5s) with no input frames (`internal/opus/drain.go`). While paused, `tick` calls `src.Drain()` on every input to release pooled buffers and skips mix/encode/output. Resumption is automatic on the next `Feed`. Saves CPU and pool churn in quiet channels.

### Gateway latency ≠ audio latency
The `gdc.bot.gateway.latency` metric (Prometheus: `gdc_bot_gateway_latency_milliseconds`, gauge, labelled `bot_id`) measures the WebSocket **control channel** heartbeat RTT only. Voice audio is transmitted over a separate **UDP** connection — gateway latency is not a measure of audio delay.

Current observed gateway latency (median per bot, `gdc_bot_gateway_latency_milliseconds`, 30d): **~103ms** (owner bot on `sealgate`) and **~134–137ms** (speaker bots on `asrock`) — i.e. **~105–140ms** across the pool. This is consistent with a geographically distant Discord region and is unrelated to the audio pipeline delay. It does however confirm that the network round-trip component of the audio path (two Discord voice server hops) contributes at least **65–90ms** of irreducible latency — the floor below which end-to-end audio latency cannot be reduced regardless of pipeline optimisations.

---

## Why Batching Does Not Help

It might seem that processing multiple queued frames per mixer tick ("batching") would let the pipeline catch up faster. In practice it does not, because of how disgo's `defaultAudioSender` paces output:

```go
// disgo/voice/audio_sender.go
s.send()   // calls ProvideOpusFrame() — BLOCKS until a frame is available
sleepTime := OpusFrameSizeMs - (time.Now().UnixMilli() - lastFrameSent)
if sleepTime > 0 {
    time.Sleep(sleepTime * time.Millisecond)
}
```

After `ProvideOpusFrame()` returns (however quickly), disgo sleeps for the remaining portion of the 20ms window before sending the next frame. Whether the mixer pre-populates 1 or 10 frames into `chOut`, disgo still pulls them at 20ms intervals. Batched output frames sit in the channel and add latency instead of reducing it.

**Root cause: goroutine count and scheduler jitter.** The buffer is not processed too slowly — it processes at exactly the correct rate (one 20ms Opus frame per 20ms tick). Latency accumulates because the Go M:N scheduler occasionally delays mixer goroutines by 2–10ms when many goroutines compete for OS threads. Reducing goroutine count, not deeper batching, is the effective remedy — see goroutine budget below.

---

## Goroutine Budget (post pull-based mixer)

For a session with 2 sources and 3 speaker channels:

| Goroutine                        | Count                                       |
|----------------------------------|---------------------------------------------|
| Fanout source goroutines         | **0** (inline on disgo's UDP goroutine)     |
| Channel mixer `Run` loops        | 3 (one per destination channel)             |
| Channel mixer `DrainWatcher`     | 3 (idle timer per channel mixer)            |
| Relay mixer `Run` loop           | 1                                           |
| Relay bridge decode goroutine    | 0–1 (only when host allows guest capture)   |
| VoiceProvider reads              | hosted in disgo sender, no extra goroutine  |
| **Total pipeline goroutines**    | **7–8**                                     |

Down from 13+ before the pull-based mixer + inline fanout decode landed.

---

## Where Time Still Goes

After Phase 3 the pipeline floor is dominated by Discord's network and frame cadence — not by anything we control in process:

| Component                                | Contribution | Reducible?                              |
|------------------------------------------|--------------|-----------------------------------------|
| Two Discord UDP hops (region-dependent)  | 65–110ms     | Pick a region closer to Discord servers |
| Discord client 20ms Opus frame buildup   | 20ms         | No — protocol-fixed                     |
| Mixer tick alignment (next 20ms boundary)| 0–20ms       | No without changing frame size          |
| Mixer queue (`SourceBuffer`)             | 0–60ms       | Already capped; lowering hurts jitter   |
| disgo sender pacing on the send side     | 0–20ms       | No — disgo enforces 20ms cadence        |

Further wins must come from reducing **stage count** (already done) or **frame size**. Frame size is a protocol choice and likely not worth touching.
