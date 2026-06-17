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
`bot_metrics.go`, `pool_metrics.go`, `session_metrics.go`, `opus_metrics.go`.
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

# active voice raids
sum(gdc_voice_sessions_active)
```
