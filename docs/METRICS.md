# Metrics — go-discord-caller

All telemetry is emitted via **OpenTelemetry** (OTLP gRPC) — there is no direct
Prometheus client. Instruments are created from a single meter in
`internal/telemetry/` and exported to the OTLP endpoint set by `OTEL_ENDPOINT`
(empty disables telemetry — see `internal/config/config.go`). The collector /
Alloy / Prometheus OTLP receiver translates each OTel instrument into one or
more Prometheus series.

This file lists every instrument by its **OTel name** (the dotted name in code)
alongside the **Prometheus series** name produced by the default OTLP→Prometheus
translation.

---

## OTel → Prometheus naming rules

The Prometheus names below assume the **default** OTLP translation (Prometheus
native OTLP receiver / OpenTelemetry Collector `prometheus` exporter, with unit
and type suffixes enabled — the defaults):

| OTel | Prometheus |
|------|------------|
| `.` and other illegal chars | `_` |
| unit `ms` | `_milliseconds` suffix |
| unit `s` | `_seconds` suffix |
| monotonic counter | `_total` suffix (not doubled if the name already ends in `total`) |
| `Float64Histogram` | `_bucket` / `_sum` / `_count` series |
| `UpDownCounter`, `ObservableUpDownCounter`, `Gauge`, `ObservableGauge` | gauge, no suffix |

> Names vary if the exporter is configured with `without_units` /
> `without_type_suffix`, or if Prometheus is run with
> `--enable-feature=otlp-deltatocumulative` style flags. The collector also adds
> a `target_info` series and `otel_scope_*` labels. Tune accordingly.

All instruments carry the resource attribute `service.name="go-discord-caller"`.

---

## 🤖 Bot / Discord entities (`internal/telemetry/bot_metrics.go`)

| OTel instrument | Type | Prometheus series | Attributes | Description |
|-----------------|------|-------------------|------------|-------------|
| `gdc.discord.guild`  | ObservableGauge | `gdc_discord_guild`  | `guild_id`, `guild_name` | Info gauge (always `1`) for known guilds. Emitted from `internal/manager/service.go`. |
| `gdc.bot.online`     | ObservableGauge | `gdc_bot_online`     | `user_id`, `guild_id`    | `1` while a bot is a registered member of the guild; absent otherwise. From `internal/manager/service.go`. |
| `gdc.voice.callers`  | ObservableUpDownCounter | `gdc_voice_callers`  | `guild_id`, `channel_id` | Non-bot users with the caller role currently in a voice channel of a guild with an active raid (host or guest), recounted from the owner bot's cache on each collection; channels with none are absent. From `internal/manager/service.go`. |
| `gdc.command.started.total` | Counter | `gdc_command_started_total` | `command`, `guild_id` ⚠️ | Slash command invocations **entering** the handler. Recorded before the handler runs, so a handler that never returns is still counted. From `internal/bot/middleware.go`. |
| `gdc.command.total`  | Counter         | `gdc_command_total`  | `command`, `guild_id` ⚠️ | Slash command invocations that **completed**. A sustained `gdc_command_started_total - gdc_command_total` means handlers went in and never came out. From `internal/bot/middleware.go`. |
| `gdc.command.duration` | Histogram (`s`) | `gdc_command_duration_seconds` (`_bucket`/`_sum`/`_count`) | `command`, `guild_id` ⚠️ | Slash command execution duration. From `internal/bot/middleware.go`. |

> ⚠️ The three `gdc.command.*` instruments label the guild as `guild.id` (dotted)
> in code — see `RecordCommand`. The OTLP→Prometheus translation rewrites this
> to `guild_id`, so dashboards still query `guild_id`, but the attribute is
> inconsistent with the `guild_id` used by every other instrument. Worth
> normalising in code.

---

## 🛰️ Speaker pool (`internal/telemetry/pool_metrics.go`)

Emitted from `internal/pool/service.go` (observable gauges via a registered
callback; counters inline in the watchdog). The `gdc.bot.verified` /
`guilds` / `members_intent` / `verification_blocked` gauges come from each
bot's own application, fetched at startup and then hourly; a failed fetch keeps
the last known value.

| OTel instrument | Type | Prometheus series | Attributes | Description |
|-----------------|------|-------------------|------------|-------------|
| `gdc.discord.bot`                | ObservableGauge | `gdc_discord_bot`                | `bot_id`, `bot_name` | Info gauge (always `1`) for known speaker bots. |
| `gdc.pool.bots.total`            | ObservableGauge | `gdc_pool_bots_total`            | —        | Total speaker bots registered in the pool. (Gauge — *not* a counter despite the `total` suffix.) |
| `gdc.pool.bots.connected`        | ObservableGauge | `gdc_pool_bots_connected`        | —        | Speaker bots with a healthy gateway connection. |
| `gdc.bot.gateway.latency`        | ObservableGauge (`ms`) | `gdc_bot_gateway_latency_milliseconds` | `bot_id` | Gateway WebSocket heartbeat RTT per bot. `0` until first ACK. See `docs/LATENCY.md` — this is the **control channel**, not audio latency. |
| `gdc.bot.verified`               | ObservableGauge | `gdc_bot_verified`               | `bot_id` | `1` when Discord shows the bot as verified, `0` otherwise. An unverified bot cannot join more than 100 servers. Owner and speakers. |
| `gdc.bot.guilds`                 | ObservableGauge | `gdc_bot_guilds`                 | `bot_id` | Approximate server count per bot, as Discord counts it against that limit. Absent when Discord reports no count. |
| `gdc.bot.members_intent`         | ObservableGauge | `gdc_bot_members_intent`         | `bot_id`, `state` | Always `1`; `state` is `approved`, `limited` (switched on without review, allowed until the bot reaches 10,000 users) or `off`. |
| `gdc.bot.verification_blocked`   | ObservableGauge | `gdc_bot_verification_blocked`   | `bot_id` | `1` when Discord has blocked verification because of unusual growth. |
| `gdc.pool.reconnect.attempts.total` | Counter | `gdc_pool_reconnect_attempts_total` | `bot_id` | Watchdog gateway reconnect attempts. |
| `gdc.pool.reconnect.failures.total` | Counter | `gdc_pool_reconnect_failures_total` | `bot_id` | Watchdog gateway reconnect failures. |

---

## 🏰 Session lifecycle & fanout (`internal/telemetry/session_metrics.go`)

Emitted from `internal/manager/voice_raid.go` (start/stop) and the auto-router
pipelines in `internal/manager/pipeline/` (route transitions); frame drops from
the opus pipeline via `FrameDropper`.

| OTel instrument | Type | Prometheus series | Attributes | Description |
|-----------------|------|-------------------|------------|-------------|
| `gdc.voice.sessions.active`         | UpDownCounter | `gdc_voice_sessions_active`         | `guild_id` | Currently active voice raid sessions. |
| `gdc.voice.session.start.total`     | Counter       | `gdc_voice_session_start_total`     | `guild_id` | Voice raid starts. |
| `gdc.voice.session.stop.total`      | Counter       | `gdc_voice_session_stop_total`      | `guild_id` | Voice raid stops. |
| `gdc.session.speakers`              | Gauge         | `gdc_session_speakers`              | `guild_id` | Speakers (pool speaker bots plus the owner bot, if it joined) in the active raid; reset to `0` on stop. |
| `gdc.fanout.frames.dropped.total`   | Counter       | `gdc_fanout_frames_dropped_total`   | `guild_id`, `path` | Opus frames dropped on full channels. `path`: `mixer` / `direct` / `channel_mixer` / `relay_bridge` / `provider` / `receiver`. |
| `gdc.session.route_transitions.total` | Counter     | `gdc_session_route_transitions_total` | `guild_id`, `from`, `to` | Auto-router source-mode transitions. `from`/`to`: `off` / `copy` / `mix`. |

---

## 🎙️ Opus / mixer timing (`internal/telemetry/opus_metrics.go`)

All histograms, attribute `guild_id`; unit `ms` unless noted. Recorded on the hot path via a
pre-baked `OpusRecorder` (`Metrics.ForGuild`) — zero-alloc per frame.

| OTel instrument | Prometheus series (`_bucket`/`_sum`/`_count`) | Buckets (ms) | Emitter | Description |
|-----------------|-----------------------------------------------|--------------|---------|-------------|
| `gdc.opus.receive.duration`    | `gdc_opus_receive_duration_milliseconds`    | 0.5, 1, 2, 5, 12, 20    | `internal/opus/voice_receiver.go` | `ReceiveOpusFrame` execution time (excludes channel-wait). |
| `gdc.opus.provide.duration`    | `gdc_opus_provide_duration_milliseconds`    | 0.5, 1, 2, 5, 12, 20    | `internal/opus/voice_provider.go` | `ProvideOpusFrame` drain+return time (excludes frame-wait). |
| `gdc.opus.allow_user.duration` | `gdc_opus_allow_user_duration_milliseconds` | 1, 2, 5, 12, 20         | `internal/manager/allow_user.go`  | Time of an `allowUser` lookup that missed the per-session cache — once per user, not per frame; cached checks are a single map read and are not recorded. |
| `gdc.mixer.tick.duration`      | `gdc_mixer_tick_duration_milliseconds`      | 0.5, 1, 2, 5, 10, 20    | `internal/opus/mixer.go`          | Mixer tick processing time. |
| `gdc.mixer.tick.lateness`      | `gdc_mixer_tick_lateness_milliseconds`      | 0.5, 1, 2, 5, 10, 20, 40, 80 | `internal/opus/mixer.go`     | How far past its 20 ms boundary a mixer tick fired. Scheduler or GC stalls show up here; they are what push a queued frame past the 60 ms cap in `gdc.mixer.pipeline.latency`. |
| `gdc.opus.copy.queue_depth`    | `gdc_opus_copy_queue_depth`                 | 0, 1, 2, 3 (frames)     | `internal/opus/voice_receiver.go` | Copy mode only (a single caller, mixer bypassed): frames already queued ahead of a copied frame in a speaker's output channel. disgo drains one frame per 20 ms, so 0 / 1 / 2 / 3 ahead ≈ under 20 / 20–40 / 40–60 / 60+ ms of wait. A frame that finds the queue full is dropped (`path="direct"`) and not recorded here. Integer histogram, unit `{frame}`, not `ms`. |
| `gdc.opus.receive.interval`    | `gdc_opus_receive_interval_milliseconds`    | 2, 5, 10, 15, 18, 22, 25, 30, 40, 60 | `internal/opus/voice_receiver.go` | Time between two consecutive frames (by RTP timestamp) from the same user as they reach the receiver. Even delivery is ~20 ms; a clump of frames lands near 0, a frame held back at 40+. Gaps across a pause in speech are not recorded. With `gdc.mixer.tick.lateness` clean, clumps here come from delivery rather than our own scheduling. |
| `gdc.opus.source.depth`        | `gdc_opus_source_depth`                     | 0, 1, 2, 3 (frames)     | `internal/opus/voice_receiver.go` | Mix mode: frames already waiting in a user's mixer input when a new frame is fed. 3 is a feed that evicted the oldest frame (`path="mixer"` drop). A distribution sitting at 1–2 is a backlog that does not drain while the user keeps talking. Integer histogram, unit `{frame}`. |
| `gdc.mixer.pipeline.latency`   | `gdc_mixer_pipeline_latency_milliseconds`   | 10, 15, 20, 25, 30, 35, 40, 50, 60, 80, 120 | `internal/opus/mixer.go`    | Latency from Discord receive to the mixer tick that consumes the frame. Floor ~20 ms (tick period), ceiling 60 ms (`audioSourceCap`). See `docs/LATENCY.md`. |

---

## 🔐 DAVE end-to-end encryption (`internal/telemetry/dave_metrics.go`)

Counted in memory by `internal/dave` and exported once per collection, so the
per-frame path stays an atomic increment. No sender identity is exported:
decrypt health is reported as a count of senders per bucket.

| OTel instrument | Type | Prometheus series | Attributes | Description |
|-----------------|------|-------------------|------------|-------------|
| `gdc.dave.decrypt.total`   | ObservableCounter | `gdc_dave_decrypt_total`   | `bot_user_id`, `outcome` (`success`/`passthrough`/`dropped`/`failure`), `reason` | Inbound frame decryption outcomes, summed over senders. |
| `gdc.dave.decrypt.senders` | ObservableGauge   | `gdc_dave_decrypt_senders` | `bot_user_id`, `health` (`clean`/`partial`/`failing`) | Distinct senders per decrypt-health bucket. One `failing` sender among `clean` ones points at that sender; `partial` everywhere points at the session. |
| `gdc.dave.encrypt.total`   | ObservableCounter | `gdc_dave_encrypt_total`   | `bot_user_id`, `outcome` (`success`/`failure`), `reason` | Outbound frame encryption outcomes. A bot whose encryptor lost its key ratchet fails every frame (`reason="missing_key_ratchet"`) and is silent in its channel while looking connected; after ~10 s of failures it rejoins its channel. |
| `gdc.dave.decrypt.position.total` | ObservableCounter | `gdc_dave_decrypt_position_total` | `bot_user_id`, `outcome` (`success`/`failure`), `position` (`first`/`early`/`mid`) | Decryptions that reached the backend, by where the frame fell in its sender's talk spurt: the first frame after ≥100 ms of silence, frames 1–9 (the first 200 ms), or later. Compare failure rates across positions to tell losses at the start of speech from losses spread through it. |

`reason` is `none` on success, otherwise one of `missing_key_ratchet`,
`invalid_nonce`, `missing_cryptor`, `decrypt_failure`, `encrypt_failure`,
`too_many_attempts` or `other`.

---

## 🖥️ Process & Go runtime (`internal/telemetry/process_metrics.go`)

The process's own resource use, for capacity planning against the container limit. Compare `gdc_process_memory_usage_bytes` with `go_memory_used_bytes`: the gap is memory libdave and libopus allocate outside the Go heap, which `GOMEMLIMIT` does not cover.

| OTel instrument | Type | Prometheus series | Attributes | Description |
|-----------------|------|-------------------|------------|-------------|
| `gdc.build.info`           | ObservableGauge         | `gdc_build_info`                     | `version`, `revision`, `go_version`, `libdave_version` | Info gauge (always `1`) for the running build. `version` is the release tag stamped by the Dockerfile (`dev` for local builds); `revision` is the short commit, `-dirty` when built with uncommitted changes. Also sent as `service.version` on every signal. |
| `gdc.process.cpu.time`     | ObservableCounter (`s`) | `gdc_process_cpu_time_seconds_total` | — | CPU time consumed by the process, user plus system. `rate()` of it is the fraction of one core in use. |
| `gdc.process.memory.usage` | ObservableGauge (`By`)  | `gdc_process_memory_usage_bytes`     | — | Resident set size, including C allocations. Linux only — absent elsewhere. |
| `go.memory.used`, `go.memory.limit`, `go.memory.gc.goal`, `go.goroutine.count`, … | ObservableGauge / Counter | `go_memory_used_bytes`, `go_memory_limit_bytes`, `go_goroutine_count`, … | `go.memory.type` on `go.memory.used` | Go runtime metrics from `go.opentelemetry.io/contrib/instrumentation/runtime`, sampled at most every 15 s. |

---

## Implementation

Instruments are defined in `internal/telemetry/`, split by subsystem:
`bot_metrics.go`, `pool_metrics.go`, `session_metrics.go`, `opus_metrics.go`,
`dave_metrics.go`, `process_metrics.go`.
`metrics.go` wires them together (`NewMetrics`); `setup.go` configures the OTLP
exporters (traces, metrics, logs) and the periodic metric reader (15 s interval).

Per-guild recorders are obtained via `Metrics.ForGuild(ctx, guildID)`
(`guild_metrics.go`), which bakes the `guild_id` attribute once and returns a
reusable `GuildMetrics` value — keeping the hot path allocation-free.

### Example PromQL

```promql
# p99 mixer pipeline latency per guild (ms)
histogram_quantile(0.99,
  sum by (guild_id, le) (rate(gdc_mixer_pipeline_latency_milliseconds_bucket[5m])))

# frame drop rate by pipeline stage
sum by (path) (rate(gdc_fanout_frames_dropped_total[5m]))

# speaker bots connected vs registered
gdc_pool_bots_connected / gdc_pool_bots_total

# bots that cannot encrypt outbound audio (silent while connected)
sum by (bot_user_id) (rate(gdc_dave_encrypt_total{outcome="failure"}[5m])) > 0

# DAVE decrypt failure rate by position in the talk spurt
sum by (position) (rate(gdc_dave_decrypt_position_total{outcome="failure"}[1h]))
  / sum by (position) (rate(gdc_dave_decrypt_position_total[1h]))

# active voice raids
sum(gdc_voice_sessions_active)
```

---

## Traces: session startup phases

`/start` takes seconds before any audio flows (measured in production: ~0.8–1.9 s
with 2 speakers, ~3 s with 5, ~3.2–4.1 s with 10), and the work is a chain of
Discord round trips. `voice.session` / `voice.session.guest` spans last as long
as the raid, so the startup cost has its own child span with one child per round
trip. `startPhase` (`internal/manager/trace.go`) opens them.

```
voice.session                        (whole raid)
└── voice.session.setup              (/start → first frame flowing)
    ├── voice.speakers.setup         speaker.candidates, speaker.joined, capture
    │   └── voice.bot.attach         bot.id, bot.kind=speaker (one per speaker, concurrent)
    │       ├── voice.conn.open      voice handshake through SessionDescription
    │       ├── voice.members.prefetch   user.count (capture modes only)
    │       ├── voice.conn.apply     provider/receiver wiring + SetSpeaking op
    │       └── voice.deaf.reconcile deaf.want, deaf.cached
    ├── voice.bot.attach             bot.kind=owner (runs after every speaker)
    │   └── … same four children
    └── voice.pipeline.build         local graph construction, expected ~0
```

What to read off a trace:

- `voice.bot.attach{bot.kind=owner}` starting only after the last speaker's
  attach ends is the serialized owner handshake.
- `voice.deaf.reconcile` spans with `deaf.cached=false` stacking one after
  another are the per-guild REST bucket serializing member PATCHes — disgo holds
  the bucket mutex across each request.
- `voice.conn.open` is the irreducible part: op4 → voice server update → WSS+TLS
  → identify/ready → UDP discovery → session description.

Everything is sampled (default `ParentBased(AlwaysSample)`). The parent
`voice.session` span only exports when the raid ends, so a trace looks
incomplete while a raid is live — the startup children are already there.
