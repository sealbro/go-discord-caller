# End-to-End Audio Latency

## Full Audio Path (e.g. `RaidModeGuildCaller` / many-callers mode)

```
User mic
  │  +20ms  Opus frame accumulation (Discord client)
  ▼
[Discord voice server]
  │  +½ RTT  voice UDP leg in (region-dependent: ~8ms in Europe, see the regional table)
  ▼
[VoiceReceiver]  decode + fanout inline (on disgo's UDP goroutine)
  │  +0.2ms allowUser lookup, Opus decode, dispatch to SourceBuffer.Feed (gdc_opus_receive_duration ~0.2ms avg)
  ▼
[SourceBuffer ring]  cap 3 frames; gdc.mixer.pipeline.latency (Prom: gdc_mixer_pipeline_latency_milliseconds)
  │  +22ms  Pull on next 20ms tick; overflow drops oldest at producer (measured: p50 22ms, p95 47ms, p99 57ms)
  ▼
[Mixer.tick()]  mix-minus, encode (0ms passthrough if single source)
  │  +<1ms  gdc_mixer_tick_duration p95 ~0.7ms, p99 ~1.5ms
  ▼
[VoiceProvider]  → UDP send
  │  +0ms   provider channel queue ≈0 (passthrough) + disgo sender
  ▼
[Discord voice server]
  │  +½ RTT  voice UDP leg out
  ▼
[Listener's Discord client]  Opus decode → speakers

Total: client legs + voice RTT + ~25ms pipeline — roughly 80–140ms in Europe (single guild, many-callers/mix mode; copy mode skips the mixer queue and is lower — see `gdc.opus.copy.queue_depth`)
```

---

## Latency Budget

| Hop                                       | Typical    | Notes                                                                                                    |
|-------------------------------------------|------------|----------------------------------------------------------------------------------------------------------|
| User mic → Discord voice server           | 20–50ms    | 20ms fixed frame accumulation + network                                                                  |
| Discord voice server → `VoiceReceiver`    | ½ voice RTT | Voice UDP leg to the host running the bot; `gdc_voice_server_latency_milliseconds` is the full round trip (~15ms in `c-ams`) |
| `allowUser` + Opus decode + fanout `Feed` | ~0.2ms     | Inline on disgo UDP goroutine, no channel hop; measured `gdc_opus_receive_duration` ~0.2ms avg          |
| `SourceBuffer` ring → `Mixer.tick`        | **22ms (p95 47ms)** | `gdc.mixer.pipeline.latency` (Prom: `gdc_mixer_pipeline_latency_milliseconds`); ceiling = `audioSourceCap`×20ms = 60ms; measured over 30 days: p50 22ms / p95 47ms / p99 57ms, mean 24ms; producer-side overflow keeps near live edge |
| Mixer tick processing + encode            | <1ms       | `gdc_mixer_tick_duration` p95 ~0.7ms, p99 ~1.5ms; 0ms on single-source passthrough                       |
| `VoiceProvider` channel queue             | ~0ms       | `gdc_opus_provide_duration` ≈0 (passthrough); drain threshold 3 frames = 60ms cap                       |
| `VoiceProvider` → Discord voice server    | ½ voice RTT | Same leg, outbound |
| Discord voice server → listener client    | 20–50ms    | |
| **Total (single guild)**                  | **~80–140ms in Europe** | Client legs (40–100ms) + voice RTT + in-process pipeline (~25ms); the bot's two legs add one full voice RTT: ~15ms in `c-ams`, ~100–127ms in the US, ~165–245ms in Asia |

**Inter-guild relay** (`ally.Session` → relay mixer → guest `VoiceProvider`) adds one more mixer stage (relay mixer tick alignment, up to 20ms plus its queue) and replaces the outbound leg with the guest guild's own voice leg, so the bot legs become half the host guild's voice RTT plus half the guest guild's.

---

## Important Design Notes

### `RaidModeOneCaller` auto-switches between copy and mix mode
`RaidModeOneCaller` no longer statically bypasses the pipeline. As of the auto-switch pipeline (commit #37), `OneCallerPipeline` (`internal/manager/pipeline/one_caller.go`) builds an **always-on mixer graph** — per-speaker-channel mixers plus the relay mixer, created up-front and started paused — and a `router` (`internal/manager/router`) flips the owner source between two modes based on the live caller count in the owner channel:

- **Copy mode** (single caller): the owner's `FanoutHandle` is installed with raw `OpusTargets` per speaker `chOut` plus an `OpusCallback` that calls `ally.Session.BroadcastFromGuild` — all inline on disgo's UDP goroutine. No decode, no mix, no re-encode, no fanout goroutine. Mixers stay paused, so the mixer queue wait above does not apply — the fast path. Its only in-process wait is the speaker's output queue, which disgo drains one frame per 20 ms; `gdc.opus.copy.queue_depth` (Prom: `gdc_opus_copy_queue_depth`) records how many frames were already queued ahead of each copied frame, so 0 / 1 / 2 ahead ≈ under 20 / 20–40 / 40–60 ms.
- **Mix mode** (multiple callers): the source decodes into the per-channel and relay mixers via `SourceBuffer`, picking up the full mixer budget above.

The `FanoutHandle` is wired by `routerInstallBuilder` (`internal/manager/pipeline/install.go`), not the old `installFanoutDirect` — that function no longer exists. `router.Recompute` decides the mode at session start (`start` closure) and on every caller join/leave, so the latency profile of a OneCaller session is dynamic, not fixed. The same copy/mix split applies to the `RaidModeOneManyGuildCaller` / `StarCaller` topologies.

### The mixer tick is the biggest fixed cost
`Mixer.Run` parks on a 20ms timer (`mixerFrameDur` in `internal/opus/mixer.go`). A frame arriving just after a tick must wait up to 20ms for the next one. This is irreducible without changing Discord's frame cadence. A tick that fires late adds to it; how late each tick fires is recorded in `gdc.mixer.tick.lateness` (Prom: `gdc_mixer_tick_lateness_milliseconds`).

### Producer-side overflow (no burst drains)
`SourceBuffer.Feed` drops the **oldest** frame on overflow (`internal/opus/source.go:51`). The previous pull-from-channel design needed an explicit drain-threshold step inside `Mixer.tick` that could discard several frames in one go (audible burst gap). Now the consumer always sees a buffer ≤ 60ms; latency cannot accumulate past that ceiling.

| Drain point                          | Mechanism                                | Max tolerated backlog |
|--------------------------------------|------------------------------------------|-----------------------|
| `SourceBuffer.Feed` (producer)       | drop oldest on full ring                 | 60ms (3 frames)       |
| `VoiceProvider.ProvideOpusFrame`     | `providerDrainThreshold` = 3 frames      | 60ms                  |

### Mixer auto-pause via `DrainWatcher`
Channel mixers pause after `DrainIdleTimeout` (5s) with no input frames (`internal/opus/drain.go`). While paused, `tick` calls `src.Drain()` on every input to release pooled buffers and skips mix/encode/output. Resumption is automatic on the next `Feed`. Saves CPU and pool churn in quiet channels.

### Gateway latency ≠ audio latency
The `gdc.bot.gateway.latency` metric (Prometheus: `gdc_bot_gateway_latency_milliseconds`, gauge, labelled `bot_id`) measures the WebSocket **control channel** heartbeat RTT only. Voice audio is transmitted over a separate **UDP** connection — gateway latency is not a measure of audio delay. The closer proxy for the audio network leg is `gdc.voice.server.latency` (`gdc_voice_server_latency_milliseconds`, labelled `guild_id`, `bot_id`, `region`): the heartbeat RTT of the voice websocket to the voice server the bot's audio is sent to.

Gateway latency (median per bot, 30d): **~105–112ms** for most bots and **~147–153ms** for the three newest, all from the same host. That host reaches its voice server in `c-ams` in ~15ms, so the gateway origin sits much farther from it than the voice servers do. This is on Discord's side and is unrelated to the audio pipeline delay; it affects the join handshake and how quickly voice-state events arrive, not audio. The gauge is reported once measured: zero before a bot's first heartbeat ACK, and absent while a later heartbeat is awaiting its ACK.

**Voice server latency by region** (`gdc_voice_server_latency_milliseconds`, 30d average) is the audio network leg. Audio crosses it twice (in and out), so one full RTT lands on every relayed word:

| Region | Avg RTT |
|--------|---------|
| `c-fra` | 11ms |
| `c-ams`, `c-cdg` | 15–17ms |
| `c-mxp`, `c-lhr`, `c-mrs`, `c-waw` | 20–28ms |
| `c-arn`, `c-hel`, `c-otp` | 41–43ms |
| `c-iad` (US East) | 101ms |
| `us-south` | 127ms |
| `c-sin` (Singapore) | 164ms |
| `c-hkg` (Hong Kong) | 198ms |
| `c-nrt` (Tokyo) | 245ms |

Guilds in Europe sit within 45ms. Guilds in the US and Asia pay 100–245ms on top of the pipeline, with peaks to ~840ms. Only a voice process in or near the guild's region reduces this; the channel's `rtc_region` setting does not, because it moves the voice server away from the users.

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
| Two Discord UDP hops (one voice RTT)     | 10–45ms in Europe, 100–245ms in the US and Asia | Only by running the voice process near the guild's region |
| Discord client 20ms Opus frame buildup   | 20ms         | No — protocol-fixed                     |
| Mixer tick alignment (next 20ms boundary)| 0–20ms       | No without changing frame size          |
| Mixer queue (`SourceBuffer`)             | 0–60ms       | Already capped; lowering hurts jitter   |
| disgo sender pacing on the send side     | 0–20ms       | No — disgo enforces 20ms cadence        |

Further wins must come from reducing **stage count** (already done) or **frame size**. Frame size is a protocol choice and likely not worth touching.
