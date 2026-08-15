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
| `UpDownCounter`, `Gauge`, `ObservableGauge` | gauge, no suffix |

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
| `gdc.voice.callers`  | UpDownCounter   | `gdc_voice_callers`  | `guild_id`, `channel_id` | Users with the caller role currently in a voice channel. From `internal/bot/handlers.go`. |
| `gdc.command.total`  | Counter         | `gdc_command_total`  | `command`, `guild_id` ⚠️ | Slash command invocations. From `internal/bot/middleware.go`. |
| `gdc.command.duration` | Histogram (`s`) | `gdc_command_duration_seconds` (`_bucket`/`_sum`/`_count`) | `command`, `guild_id` ⚠️ | Slash command execution duration. From `internal/bot/middleware.go`. |

> ⚠️ The two `gdc.command.*` instruments label the guild as `guild.id` (dotted)
> in code — see `RecordCommand`. The OTLP→Prometheus translation rewrites this
> to `guild_id`, so dashboards still query `guild_id`, but the attribute is
> inconsistent with the `guild_id` used by every other instrument. Worth
> normalising in code.

---

## 🛰️ Speaker pool (`internal/telemetry/pool_metrics.go`)

Emitted from `internal/pool/service.go` (observable gauges via a registered
callback; counters inline in the watchdog).

| OTel instrument | Type | Prometheus series | Attributes | Description |
|-----------------|------|-------------------|------------|-------------|
| `gdc.discord.bot`                | ObservableGauge | `gdc_discord_bot`                | `bot_id`, `bot_name` | Info gauge (always `1`) for known speaker bots. |
| `gdc.pool.bots.total`            | ObservableGauge | `gdc_pool_bots_total`            | —        | Total speaker bots registered in the pool. (Gauge — *not* a counter despite the `total` suffix.) |
| `gdc.pool.bots.connected`        | ObservableGauge | `gdc_pool_bots_connected`        | —        | Speaker bots with a healthy gateway connection. |
| `gdc.bot.gateway.latency`        | ObservableGauge (`ms`) | `gdc_bot_gateway_latency_milliseconds` | `bot_id` | Gateway WebSocket heartbeat RTT per bot. `0` until first ACK. See `docs/LATENCY.md` — this is the **control channel**, not audio latency. |
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
| `gdc.session.speakers`              | Gauge         | `gdc_session_speakers`              | `guild_id` | Speaker bots that joined the active raid; reset to `0` on stop. |
| `gdc.fanout.frames.dropped.total`   | Counter       | `gdc_fanout_frames_dropped_total`   | `guild_id`, `path` | Opus frames dropped on full channels. `path`: `mixer` / `direct` / `channel_mixer` / `relay_bridge` / `provider` / `receiver`. |
| `gdc.session.route_transitions.total` | Counter     | `gdc_session_route_transitions_total` | `guild_id`, `from`, `to` | Auto-router source-mode transitions. `from`/`to`: `off` / `copy` / `mix`. |

---

## 🔐 DAVE / E2EE voice (`internal/telemetry/dave_metrics.go`)

**Only emitted when `DAVE_IMPL=dave-go`.** The default `libdave` backend exposes
no counters, so no callback is registered and none of these series exist — their
absence is expected, not a failure.

Every instrument is observable: the counters live inside the DAVE sessions and
are polled by the callback registered in `internal/dave/registry.go`. Values are
summed across all sessions of the process (live ones plus the final values of
sessions already closed), so they stay monotonic as connections come and go —
there are deliberately **no per-session or per-guild attributes**, which would
churn one series per voice join.

| OTel instrument | Type | Prometheus series | Attributes | Description |
|-----------------|------|-------------------|------------|-------------|
| `gdc.dave.commits.total`                | ObservableCounter | `gdc_dave_commits_total`                | `result`=`processed`\|`failed` | MLS commits handled. `failed` is the counterpart of the `bad_optional_access` failures in [#43](https://github.com/sealbro/go-discord-caller/issues/43). |
| `gdc.dave.welcomes.total`               | ObservableCounter | `gdc_dave_welcomes_total`               | `result`=`joined`\|`failed` | MLS welcomes handled. |
| `gdc.dave.recoveries.total`             | ObservableCounter | `gdc_dave_recoveries_total`             | `cause`=`mls`\|`transport` | Session recoveries armed. `mls` = the crypto handshake broke; `transport` = the voice gateway was down. Only `mls` implicates the DAVE stack. |
| `gdc.dave.frame.failures.total`         | ObservableCounter | `gdc_dave_frame_failures_total`         | `op`=`encrypt`\|`decrypt` | Frames that failed crypto. Bursts right after a join/move are protocol-normal; sustained growth is not. |
| `gdc.dave.frames.total`                 | ObservableCounter | `gdc_dave_frames_total`                 | `kind`=`passthrough`\|`transition` | `passthrough` = no epoch active; `transition` = encrypted with the *retained previous* ratchet during a re-key. |
| `gdc.dave.transition.windows.total`     | ObservableCounter | `gdc_dave_transition_windows_total`     | — | Re-key windows entered (one per epoch activation with a ratchet to retain). |
| `gdc.dave.rejections.total`             | ObservableCounter | `gdc_dave_rejections_total`             | `kind`=`replay`\|`proposal` | Inputs refused by validation. Non-zero means a diverged peer, a duplicate, or something probing the session. |
| `gdc.dave.downgrades.total`             | ObservableCounter | `gdc_dave_downgrades_total`             | — | Downgrades from E2EE to transport-only (a non-supporting client joined). |
| `gdc.dave.degraded.seconds.total`       | ObservableCounter (`s`) | `gdc_dave_degraded_seconds_total`  | — | Time spent without an active epoch, across recoveries that succeeded. **The direct measure of the outage in #43.** |
| `gdc.dave.transport.retry.seconds.total`| ObservableCounter (`s`) | `gdc_dave_transport_retry_seconds_total` | — | Time spent in send-retry backoff with the voice gateway down. |
| `gdc.dave.sessions.live`                | ObservableGauge | `gdc_dave_sessions_live`                | — | Sessions currently attached to a voice connection. |
| `gdc.dave.sessions.degraded`            | ObservableGauge | `gdc_dave_sessions_degraded`            | — | Live sessions that expect E2EE but have no active epoch *right now*. Protocol-version-0 channels are excluded — they never get an epoch by design. |

`gdc_dave_frames_total{kind="transition"}` is the metric that shows the reason
for running `dave-go` at all: those are frames carried through a re-key window
that the default backend would have dropped.

---

## 🎙️ Opus / mixer timing (`internal/telemetry/opus_metrics.go`)

All histograms, unit `ms`, attribute `guild_id`. Recorded on the hot path via a
pre-baked `OpusRecorder` (`Metrics.ForGuild`) — zero-alloc per frame.

| OTel instrument | Prometheus series (`_bucket`/`_sum`/`_count`) | Buckets (ms) | Emitter | Description |
|-----------------|-----------------------------------------------|--------------|---------|-------------|
| `gdc.opus.receive.duration`    | `gdc_opus_receive_duration_milliseconds`    | 0.5, 1, 2, 5, 12, 20    | `internal/opus/voice_receiver.go` | `ReceiveOpusFrame` execution time (excludes channel-wait). |
| `gdc.opus.provide.duration`    | `gdc_opus_provide_duration_milliseconds`    | 0.5, 1, 2, 5, 12, 20    | `internal/opus/voice_provider.go` | `ProvideOpusFrame` drain+return time (excludes frame-wait). |
| `gdc.opus.allow_user.duration` | `gdc_opus_allow_user_duration_milliseconds` | 1, 2, 5, 12, 20         | `internal/manager/allow_user.go`  | `allowUser` filter time per evaluated frame. |
| `gdc.mixer.tick.duration`      | `gdc_mixer_tick_duration_milliseconds`      | 0.5, 1, 2, 5, 10, 20    | `internal/opus/mixer.go`          | Mixer tick processing time. |
| `gdc.mixer.pipeline.latency`   | `gdc_mixer_pipeline_latency_milliseconds`   | 10, 30, 50, 70, 100, 200, 500 | `internal/opus/mixer.go`    | End-to-end latency from fanout decode to mixer output. See `docs/LATENCY.md`. |

---

## Implementation

Instruments are defined in `internal/telemetry/`, split by subsystem:
`bot_metrics.go`, `pool_metrics.go`, `session_metrics.go`, `opus_metrics.go`,
`dave_metrics.go`.
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

# time DAVE sessions spent without encryption keys (dave-go backend only)
rate(gdc_dave_degraded_seconds_total[5m])

# frames rescued by the retained ratchet during MLS re-keys
rate(gdc_dave_frames_total{kind="transition"}[5m])

# MLS handshake failures vs. mere network blips
sum by (cause) (rate(gdc_dave_recoveries_total[5m]))

# active voice raids
sum(gdc_voice_sessions_active)
```
