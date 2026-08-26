# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.9.6] - 2026-08-26

### Added
- **Turkish language support**: the bot's slash commands, setup UI, and status messages can now be displayed in Turkish. Thanks to first-time contributor @hasanunlukilinc for this! ([#54](https://github.com/sealbro/go-discord-caller/pull/54))
- **Raid mode dashboard tracking**: monitoring now shows which audio mode (One Caller / Many Callers / One↔Many) each active voice raid is running, per server.

### Changed
- **Clearer voice diagnostics logs**: log lines from the voice layer are now tagged with the bot they came from, so an issue with one bot's voice connection can be told apart from another's instead of looking identical in the logs.



### Fixed
- **Listeners going silent a few seconds into a raid**: with two or more people talking, a pause longer than five seconds cut the audio for good. It now resumes on its own.
- **Guest servers losing the host's audio after a quiet spell** ([#51](https://github.com/sealbro/go-discord-caller/issues/51)): a lull could permanently cut audio to the guest server while the host still heard the guest.
- **Bot crashing when a speaker failed to join**: a speaker timing out on its voice channel could take the whole bot down, along with every other server's session.
- **Unhelpful error when joining a relay code on an unconfigured server**: the failure blamed offline bots; it now points to `/setup` → **Bind Speakers**.
- **Wrong caller counts on the dashboards**: channel moves went uncounted, so the figures drifted down over time. Restart once after upgrading to clear existing drift.
- **Missing and broken monitoring panels**: gateway latency could fail on bots still starting up, and the speaker reconnect panels showed "No data" instead of zero.

### Changed
- **Pinned away from known-bad library versions**: excluded three Discord library releases that caused bots to drop and reconnect repeatedly.
- **Dependency refresh**: updated gRPC and related Go modules; no change in behaviour.

## [0.9.4] - 2026-08-15

### Fixed
- **No slash commands after inviting the bot to a new server** ([#46](https://github.com/sealbro/go-discord-caller/issues/46)): commands were only registered for servers the bot was already in when it started, so a freshly invited server saw a bot with no commands at all until the operator restarted it. Commands are now registered as soon as the bot joins.

### Changed
- **Dependency refresh**: updated the OpenTelemetry stack and related Go modules to current versions; no change in behaviour for operators.

## [0.9.3] - 2026-08-13

### Fixed
- **Speakers going silent part-way through a session**: when a bot's voice connection quietly re-established itself in the background, the bot stayed sitting in its channel but stopped both capturing and playing audio, with nothing in the logs to show for it — the only way back was to move the bot out of the channel and in again. Audio is now restored automatically when this happens, and because the bot never leaves the channel there is no join/leave sound: listeners hear a short gap instead of permanent silence.
- **Leftover connections after a failed join**: if a speaker timed out joining its channel, the half-finished connection was left behind and could be picked up by the next raid, which then started in a broken state. Failed joins are now cleaned up properly.

### Changed
- **Voice diagnostics reach the logs**: warnings from the voice layer, including connection drops and their close codes, were previously discarded outright. They are now exported at warning level, so audio outages leave a trace that can be investigated.

## [0.9.2] - 2026-08-01

### Changed
- **Presence-based idle detection**: a voice raid now auto-stops based on whether its channels still have real (non-bot) users connected, instead of inferring silence from mixer audio activity — so forgotten raids shut down reliably even when audio was still being relayed to an empty room. The `SESSION_IDLE_TIMEOUT` setting and default (`10m`) are unchanged.
- **Owner bot on the latency dashboards**: the owner/caller bot's gateway round-trip time is now reported alongside the speaker pool, and the online-bots panel labels bots by name rather than raw ID.
- **Dependency refresh**: updated Go modules (Opus binding, gRPC) to current versions.

## [0.9.1] - 2026-06-21

### Changed
- **Dependency refresh**: updated Go modules to current versions.

### Fixed
- **Voice processing data races**: concurrent access to the member cache during audio capture is now mutex-guarded, eliminating intermittent races that could surface under the race detector and during fast caller churn.
- **Session idle timeout reliability**: the idle watcher now uses a dedicated idle probe to detect silence, so forgotten raids auto-stop dependably instead of occasionally lingering.

## [0.9.0] - 2026-06-16

### Added
- **Auto-routing for voice channels**: each captured channel now flips between raw-Opus copy and decoded-mix modes based on the live count of role-bearing callers. One-caller channels skip the mixer entirely (lower CPU and audio latency), while channels with two or more callers continue to mix cleanly. Transitions follow voice join/leave/move and role changes with a short debounce so bursts of events do not thrash the pipeline.
- **Route-transition metric**: new counter `gdc.session.route_transitions.total` (labels: `guild_id`, `from`, `to`) lets dashboards spot stuck or thrashing routes per guild.

### Changed
- **Lower-latency mixer and provider**: the mixer tick now reads its inputs from a lock-free snapshot, and the voice provider hard-drains when the playback queue runs deep — cutting tail latency and avoiding the slow-consumer fallback under burst load.
- **Auto-router replaces the manual pause/resume path**: all voice and member events fan into a single debounced re-evaluation, so per-channel mixer pause/resume and copy↔mix transitions stay coherent even under fast caller churn.
- **Internal restructure into `router` and `pipeline` sub-packages**: voice-raid wiring split out of `manager/` into focused packages. Behaviour unchanged; integrators that imported the unexported pipeline types must move to the new exported `pipeline.Params` / `pipeline.HostFor` / `pipeline.GuestFor` surface.

### Fixed
- **"Robo voice" with two or more users in the same channel**: the receiver shared one Opus decoder across all speakers, so its state ping-ponged between users and corrupted the decoded PCM. Each speaker now has its own decoder.
- **Mixer clock drift**: the per-tick timer fell behind Discord's 50 Hz cadence, filled the per-source ring, and dropped frames. The tick now advances against an absolute wallclock deadline.
- **Mixer re-encode quality**: switched to `hraban.AppAudio`, raised bitrate 48→64 kbps and complexity 5→8, disabled DTX, and pre-attenuated the PCM sum by 1/√N — removes clip-and-distort when multiple speakers peak together.

## [0.8.2] - 2026-06-09

### Added
- **Session idle timeout**: voice raids now auto-stop after a configurable period of continuous silence, freeing speaker bots when a session is forgotten. Tunable via `SESSION_IDLE_TIMEOUT` (default `10m`; set `0` to disable).
- **Paused-frame diagnostics**: new metrics track how often mixers and per-source buffers stay paused, making it easier to spot stuck or under-running channels in dashboards.

### Changed
- **Voice connection pipeline streamlined**: legacy capture channels removed and buffer handling reworked, reducing internal hops between receiver and provider for lower audio latency.
- **Go 1.26.4 + dependency refresh**: toolchain updated and indirect deps bumped.
- **Clearer onboarding docs**: README now spells out the owner-bot invitation flow step by step.

### Fixed
- **Speaker reconnect race after admin-move**: when an admin dragged a speaker bot to a different voice channel, the owner-driven reconnect could race the speaker bot's own gateway listener and end up opening to the wrong channel, timing the reconnect out. The reconnect now briefly waits for the speaker's own listener to acknowledge the move before tearing down the old voice connection, so audio resumes within a few seconds.

## [0.8.1] - 2026-05-25

### Added
- **Mixer idle-pause**: mixers auto-pause after 5 s of silence and resume when audio arrives, saving CPU in quiet channels
- **Provider/receiver middleware**: voice provider and receiver pipelines now accept middleware hooks for future use (recording, transcription, etc.)

### Changed
- **Lower audio latency**: mixer inputs replaced with ring buffers polled each tick; worst-case queue depth drops from ~100 ms to ≤ 60 ms
- **Go 1.26.3 + multi-arch Docker**: toolchain updated; Docker release images now build for `linux/amd64` and `linux/arm64`

## [0.8.0] - 2026-05-24

### Added
- **Multi-language support (7 locales)**: every user-facing string — slash command descriptions, ephemeral responses, status output, raid mode labels, and `/setup` UI — is now translated into English, Russian, Spanish, German, French, Brazilian Portuguese, and Polish. CLDR plural forms handle Russian and Polish noun declension. Translations are embedded into the binary via `go:embed`, so no runtime files are required
- **Per-guild language pin via `/setup`**: server admins can pin the bot's response language from a dropdown on the main setup page; the choice is persisted in the YAML store. Falls back to each user's Discord client locale when unpinned, and to English when the user's client locale is not in the bundle
- **i18n test guards**: a bundle parity test fails CI if any non-English locale file is missing a key from `en.yaml`; a Discord-limits test validates that every command/option/choice name and description (across all locales) fits Discord's documented length limits — catches translation overruns before they reach Discord's command sync

### Changed
- **`Status.String()` → `Status.Render(loc)` and `RaidMode.Pretty()` → `Pretty(loc)`**: both renderers now take an `i18n.Translator` so status output and mode labels render in the active locale; passing `nil` falls back to English (used for logs and tests). Slash command descriptions also gain `DescriptionLocalizations` so Discord renders them per user automatically
- **`bot.Commands` global → `BuildCommands(bundle)` factory**: the slash command list is now built at runtime from the i18n bundle so localizations stay in lockstep with the YAML files

### Removed
- **`/bind-role` and `/bind-manager-role` slash commands**: these duplicated functionality already covered by the `/setup` → "Bind Roles" sub-page, where role-select menus are pre-filled with the current binding. The interactive flow remains the only way to bind roles, removing the unused `omit` dependency and 42 dead translation strings (6 keys × 7 locales)

## [0.7.5] - 2026-05-09

### Added
- **Tests**: integration tests for all modes, unit tests for `YAMLStore`, `RaidMode`, `Mixer`, and `bot` install-URL helpers; `Makefile` with `test`, `test-race`, and `bench` targets

### Changed
- **Mixer audio quality improved**: relay bitrate raised from 16 Kbps to 48 Kbps and encoder complexity increased from 3 to 5 — relay audio is noticeably cleaner in multi-source sessions with only modest additional CPU cost

### Fixed
- **Opus buffer data race in multi-target fanout**: `VoiceReceiver.dispatchFanout` previously shared a single pooled buffer across all fanout targets; each `VoiceProvider` would independently call `PutEncodedFrame`, causing double-returns to the pool and potential data races under concurrent sends — each target now receives its own copy of the buffer

## [0.7.4] - 2026-05-01

### Changed
- **`GuildMetrics` consolidates per-guild metric handles**: a single `Metrics.ForGuild(ctx, guildID)` call now produces a `GuildMetrics` value that carries the baked-in guild ID, OpusRecorder, and FrameDropper factories — removing the repeated `Opus.For(guildID).WithDrop(...)` construction at every pipeline call site and cutting per-session allocation overhead
- **Inline fanout replaces per-source goroutines**: Opus frames are now dispatched to mixer inputs directly inside `ReceiveOpusFrame` on disgo's UDP read goroutine via `FanoutHandle`, eliminating the dedicated per-source decode goroutines and one buffered-channel hop that previously sat between receiver and mixers — lowers end-to-end audio latency
- **Mixer output channel replaced by sink callback**: mixer output is now a sink callback function instead of a buffered `chan []byte`, removing an additional channel hop between the mixer and `VoiceProvider`
- **Grafana dashboard queries include `guild_id`**: all panel metric expressions now filter and group by `guild_id`, enabling per-guild breakdown in the Grafana dashboard

## [0.7.3] - 2026-04-27

### Added
- **`AllowFilter` permission cache**: per-user allow decisions are cached in a `sync.Map` and updated from `VOICE_STATE_UPDATE` / `onGuildMemberUpdate` events; the hot path performs a single atomic load instead of a disgo cache mutex read on every Opus frame
- **Voice pipeline metrics** (`gdc.opus.*`, `gdc.mixer.*`): OTel histograms for `ReceiveOpusFrame` duration, `ProvideOpusFrame` drain path, `allowUser` filter latency, mixer tick duration, and end-to-end pipeline latency; all instruments carry a pre-baked `guild_id` attribute to keep cardinality bounded
- **Voice raid pipeline topologies**: `voice_raid_pipeline.go` introduces explicit pipeline builders for `OneCaller`, `OneManyGuildCaller`, and `OneManyAllyCaller` topologies; each topology is wired at session start rather than decided per-frame, reducing branching on the audio hot path
- **Bot voice channel movement detection**: owner and speaker bots now detect when Discord moves them to a different channel mid-session and reconnect automatically, preventing silent audio loss

### Changed
- **Async metrics recording in `VoiceReceiver`**: `RecordReceive` is no longer called inline on the Opus hot path; duration samples are sent non-blocking to a buffered `chan float64` and drained by a background goroutine — eliminates OTel histogram mutex contention as a source of multi-millisecond latency spikes
- **Pool buffers use fixed-size array pointers**: `recvFramePool`, `encodedFramePool`, and `pcmPool` now store `*[N]T` instead of `*[]T`; each pool miss is one heap allocation instead of two (backing array + slice header), and `PutEncodedFrame` / `PutPCM` avoid escaping the slice header to the heap on every return
- **`MixerMetrics` replaced by `OpusMetrics`**: metrics are now created once per session with the guild ID pre-baked via `OpusMetrics.For(guildID)`, removing per-call attribute allocation from every `Record` call
- **`mixerOutputBuf` reduced from 10 to 5**: output channel depth lowered to 100 ms; together with the drain-on-threshold logic in `VoiceProvider` this keeps end-to-end audio latency tighter

### Fixed
- **Only caller audio relayed to guest guilds**: speaker bot audio was incorrectly included in the inter-guild relay stream; relay mixer now receives frames exclusively from the owner capture path
- **`gdc_fanout_frames_dropped_total` and `gdc_discord_guild` metrics**: counters were not wired up to their instruments correctly and reported zero; label sets and registration now match the instruments
- **Reconnect logic used stale context**: `FrameDroppers` and metrics recorders were rebuilt with a cancelled context on reconnect; they now receive the live session context so cleanup fires correctly on next stop
- **Buffer management for encoded frames**: `PutEncodedFrame` now correctly identifies and returns both `encodedFrameCap` and `recvFrameCap` buffers to their respective pools; previously some buffers were silently dropped, reducing pool hit rate under load

## [0.7.2] - 2026-04-25

### Added
- **Bot channel access check before raid start**: `/start` now verifies that every enabled, bound bot has `ViewChannel + Connect + Speak` permissions in its target channel before connecting; a formatted warning message listing the offending bot and channel is sent and the raid is aborted if any permission is missing
- **Startup cache warmup per guild**: channels and bot members (owner + all pool speakers) are fetched via REST at startup and populated into the owner bot's cache so permission checks are fully cache-based with no per-call REST round-trips
- **Gateway latency metric** (`gdc.pool.gateway.latency`): round-trip latency for each speaker bot gateway connection exported as an OTel histogram
- **`gdc_bot_online` metric**: observable gauge emitted for each bot that is an active guild member; owner bot is always included, speaker bots only when registered in the guild — keeps cardinality bounded
- **Grafana dashboard**: initial dashboard for go-discord-caller covering pool bots, session speakers, guild metrics, and audio pipeline telemetry; dashboard file moved to `misc/dashboards/`

### Changed
- **Session metrics on voice state events**: active session speaker count is updated on `VOICE_STATE_UPDATE` in addition to session start/stop
- **`disgo` log level from config**: the disgo internal logger now honours `LOG_LEVEL` env var instead of always using the default level

### Fixed
- **`ViewChannel` permission not checked**: previous access check only verified `Connect + Speak`; denying "View Channel" in the Discord UI was silently ignored; all three permissions are now required

## [0.7.1] - 2026-04-22

### Added
- **Direct passthrough for `OneCaller` mode**: raw Opus bytes flow from the owner capture channel straight to all speaker outputs and the relay session — the entire mixer pipeline (decode, mix, encode) is skipped; zero CPU overhead for the single-caller case
- **Star topology direct output for `OneManyGuildCaller`**: owner Opus is forwarded raw to each speaker channel (no re-encode); decode happens only once for the relay mixer; per-speaker-channel mixers are eliminated
- **Guest direct output for `OneManyAllyCaller`**: guest `JoinSession` skips per-channel mixer creation and delivers relay audio directly to speaker outputs

### Changed
- **PCM buffer pool** (`GetPCM`/`PutPCM`): fanout goroutines and mixer tick now recycle `[]int16` PCM buffers via `sync.Pool`; superseded frames during drain and paused-mixer frames are returned immediately
- **Encoded frame pool** (`getEncodedFrame`/`PutEncodedFrame`): re-encoded Opus output buffers are recycled via `sync.Pool`; `VoiceProvider` returns each buffer before blocking for the next frame, eliminating ~50–200 allocations/sec across active mixers; pool capacity derived from `mixerBitrate` and `mixerFrameSize` (4× nominal CBR frame size)
- **Single-source passthrough copy removed**: `Mixer.tick` forwards `Frame.Opus` directly to the output channel instead of making a defensive copy — the fanout goroutine already produces an isolated copy per frame
- **Relay bridge consolidated**: `registerRelayInputs` now registers one shared Opus input channel and decodes each packet once, fanning the resulting `Frame` to all destination mixers; previously one decoder goroutine per destination channel
- **Relay bridge drain threshold** (`relayBridgeDrainThreshold = 3`): drain-to-latest only activates when > 3 packets are queued; previously always drained unconditionally
- **Buffer depths reduced**: `audioChanBuf` 50 → 10, `mixerOutputBuf` 50 → 10, `mixerInputDrainThreshold` 20 → 4, `providerDrainThreshold` 5 → 3 — all aligned to drain thresholds so latency caps at ~200 ms instead of accumulating silently
- **Provider drain preserves last frame**: `VoiceProvider` drain loop stops at `len(ch) > 1` (keeps one queued frame) instead of draining to empty, preventing mid-word audio cuts

## [0.7.0] - 2026-04-21

### Added
- **`mode:one-many` for `/start`**: new raid mode where the owner hears all speakers but speakers only hear the owner — useful for large raids where cross-talk between speaker channels is unwanted
- **Automatic audio pause on empty channels**: bots stop processing audio for a channel when no real users are listening, reducing idle CPU usage
- **Audio pipeline metrics**: mixer processing time and end-to-end audio latency are now exported as OTel metrics — `gdc.mixer.tick.duration` and `gdc.mixer.pipeline.latency`

### Changed
- **Lower audio latency**: stale audio frames are dropped when the pipeline falls behind, keeping audio close to real-time instead of playing back an accumulated delay
- **More reliable config persistence**: bot settings are now saved atomically, preventing corruption if the process is interrupted during a write

### Fixed
- **Spurious warnings on startup**: a harmless log warning that appeared on every clean boot has been removed

## [0.6.1] - 2026-04-20

### Added
- **`LOG_LEVEL` env var**: controls the minimum log level for both the stdout `slog` text handler and the OTel log bridge; accepted values: `debug`, `info`, `warn`, `error` (default: `info`); previously the level was hardcoded to `info` with no way to enable debug output at runtime
- **OTel log bridge level gating**: `telemetry.Setup` now wraps the `otelslog` handler in a `levelHandler` that short-circuits `Enabled()` at the configured level — prevents below-threshold records from being serialised and shipped via OTLP even when the bridge is active

### Fixed
- **Guest AllyCaller owner bot not capturing audio**: in `JoinSession` the owner bot was wired with `WithVoiceProvider` only — its capture channel was silently discarded and `EmptyVoiceReceiver` was used, so users speaking in the guest owner's channel were never relayed to the host; now `WithVoiceReceiver` is added and the capture channel is included as a source in `wireFanout`, mirroring the host `StartVoiceRaid` setup

### Changed
- **Mixer: decode once per source** (`wireFanout`): each incoming Opus packet is decoded to PCM exactly once in the fanout goroutine and the resulting `opus.Frame` (PCM + raw Opus bytes) is distributed to all downstream mixers; previously each mixer held its own `hraban.Decoder` and decoded independently — with 3 sources × 3 mixers this cuts decodes from 9 → 3 per 20 ms tick; decode scratch buffer moves from `Mixer` struct to each per-source goroutine (supersedes 0.6.0 pre-allocation in `NewMixer`)
- **Mixer: PLC removed**: 0.6.0 introduced a per-input silent-tick counter to skip Opus PLC after 25 ticks (~500 ms); with decoders gone from the mixer, PLC is eliminated entirely — a silent source simply contributes zero samples each tick
- **Mixer: single-source encode bypass**: when only one source is active the mixer forwards the original Opus packet directly, skipping the encode step entirely; the common single-speaker case now costs one `copy` instead of a full `Encode` call
- **Mixer: Timer-based tick** (was Ticker): `Run` now resets a `time.Timer` after each tick completes, preventing stale-frame back-pressure when a tick takes longer than 20 ms under load
- **Mixer: encoder tuned for voice relay**: bitrate set to 16 kbps (was ~32 kbps default), in-band FEC enabled, packet-loss percentage set to 5 % — smaller frames, built-in loss resilience; `SetVBR` omitted (not exposed by hraban/opus wrapper)
- **Mixer: 4× unrolled PCM accumulation loop**: inner accumulation loop unrolled to hint the compiler toward SIMD emission (SSE2/AVX2/NEON); `mixerPCMBuf` (1920) is always divisible by 4 so no tail loop is needed
- **Fanout read-only contract documented**: `wireFanout` carries an explicit comment that `Frame.Opus` is shared read-only across all targets; the mixer's single-source path copies it before forwarding downstream

## [0.6.0] - 2026-04-19

### Added
- **OpenTelemetry observability**: all three signals — traces, metrics, and logs — exported via OTLP gRPC to a single configurable endpoint; set `OTEL_ENDPOINT` (e.g. `alloy:4317`) to enable, leave empty to disable (no-op)
- **Distributed tracing**: `discord.command` span wraps every slash command execution with `command`, `guild.id`, and `user.id` attributes; `voice.session` and `voice.session.guest` spans track the full lifetime of host and guest voice raid sessions
- **Metrics**: `gdc.command.duration` (histogram), `gdc.command.total`, `gdc.voice.sessions.active`, `gdc.voice.session.start.total`, `gdc.voice.session.stop.total`
- **OTel log bridge**: when `OTEL_ENDPOINT` is set, the default `slog` logger is replaced with the `otelslog` bridge so all log lines are exported via OTLP and automatically carry `trace_id`/`span_id` when a context is provided
- **`onGuildMemberUpdate` event listener**: overwrites the member cache on every role change so `allowUser` picks up new role assignments on the very next audio frame without requiring a session restart

### Changed
- **Mixer CPU reduction (~60%)**: Opus encoder complexity lowered from 9 to 3; DTX enabled; PLC decodes skipped after 25 consecutive silent ticks (~500 ms) per input — avoids expensive PLC calls during sustained silence
- **Mixer allocations eliminated**: PCM accumulator, decode scratch buffer, and encode buffer are now pre-allocated once in `NewMixer` and reused on every tick via `clear()`; only the final encoded output slice is allocated per frame
- `bot.Run` now accepts a `context.Context`; the owner signal handler (`SIGINT`/`SIGTERM`) is set up in `main` via `signal.NotifyContext` and passed in — `bot.New` performs no network I/O
- All `slog` call sites updated to `slog.*Context` variants to carry trace context through to the log bridge

## [0.5.0] - 2026-04-16

### Added
- **`/start mode:` option**: `/start` now accepts an optional `mode` parameter with two choices:
  - `one` *(default)* — only the designated caller is captured; all speakers play back in listen-only mode
  - `many` — every bound speaker channel becomes a two-way participant; users in each channel hear the mixed audio of all other channels (mix-minus)
- **Multi-source voice mixing (mix-minus)**: When using `mode:many`, multiple users can speak simultaneously across different channels and all hear each other without echo — each channel only receives audio from *other* channels, never its own audio reflected back
- **Guild Caller mode (`/start mode:many`)**: Speaker bots in every bound channel both capture and play back audio, turning the whole speaker pool into a multi-channel conference relay
- **Guest listener mode by default**: A guest guild that joins with `/start code:XXXXXX` (no `mode` option) is in **listener-only** mode — it receives all host audio but its own users are not captured; add `mode:many` to enable capture on the guest side (only effective when the host also uses `mode:many`)
- **Relay mixer for inter-guild audio**: All audio sources are continuously mixed into a single relay stream broadcast to guest guilds, so guests hear the full conversation regardless of how many callers are active on the host
- **Voice Flow documentation**: New [VOICE_FLOW.md](docs/VOICE_FLOW.md) with detailed signal flow diagrams for each raid mode


## [0.4.1] - 2026-04-10

### Fixed
- **`allowUser` blocking on audio frames**: replaced per-frame REST `GetMember` fallback with a non-blocking approach — a single `RequestMembers` gateway op at session start pre-fetches all users currently in the owner's voice channel; `onVoiceJoin` overwrites the cache with the full member (including `RoleIDs`) from the event, preventing stale partial entries written by `VOICE_STATE_UPDATE`

### Changed
- `internal/discache` package removed — disgo's built-in `cache.NewCache` / `cache.NewGroupedCache` are identical implementations; `cache.WithMemberCache`, `cache.WithRoleCache`, and `cache.WithGuildCache` removed from bot wiring since disgo creates them automatically when only `cache.WithCaches(cache.FlagsAll)` is set
- Audio frame channel buffer size extracted to `audioChanBuf` constant in `manager/service.go`

## [0.4.0] - 2026-04-09

### Added
- **Inter-guild relay (`/start code:XXXXXXXX`)**: a guild can join another guild's active voice raid as a guest — the owner bot relays audio from the host and all enabled speakers join their bound channels; the session tears down automatically when the host stops or the guest cancels
- **Persistent relay codes**: each guild is assigned a unique 8-character base32 relay code stored in YAML; codes are generated on first guild seed (startup or bot join) and guaranteed collision-free across all guilds; visible in `/status`
- **Guest owner relay**: when a guest guild joins a session, its owner bot also connects to its bound channel as a voice relay (not just the speaker bots), enabling full fan-out on the guest side
- **Guest guild names in host `/status`**: the host's Voice Raid line now lists the names of all connected guest guilds, e.g. `🔴 active (3 speakers joined) — guests: GuestServer1, GuestServer2`
- **`/status` guild and host names**: guild display name shown at the top of every status message; guest sessions show the host guild name next to the relay code
- **Speaker bind menu — range navigation buttons**: Prev/Next replaced with labeled page-range jump buttons (`1-3`, `4-6`, …); the current page is highlighted (primary, disabled); a sliding window of 4 keeps the row within Discord's 5-button limit for large pools
- **`internal/discache` package**: `FlatCache[T]` and `GroupedCache[T]` extracted from the `bot` package into a reusable `internal/discache` package; both implement the corresponding `disgo/cache` interfaces with compile-time assertions
- **Guild cache wired**: owner bot now uses a custom `FlatCache[discord.Guild]` via `cache.WithGuildCache`, enabling guild name lookups from `ownerClient.Caches.Guild()`

### Changed
- `StartVoiceRaid` now returns `(relay.RelayCode, error)` instead of `error`; the relay code is derived from the persistent per-guild store value
- `JoinChannel` signature changed to `(ctx, guildID, userID)` — bound channel lookup moved inside the method; callers no longer need to look up the channel ID themselves
- `SeedGuild` is now called on `GUILD_CREATE` events (bot joining a new server) to seed speakers and ensure a relay code is generated immediately
- `relay.RelayCode` introduced as a named type alias for `string` used throughout the API
- `groupedCache` TTL (`cacheEntity` wrapper) removed — the 5-minute eviction was silently causing `Caches.Member()` to return `false`, blocking callers from speaking after the TTL expired

## [0.3.0] - 2026-04-03

### Added
- **Setup UI — "Bind Roles" page**: capture role and manager role selectors extracted from the main setup menu into a dedicated sub-page, navigated via a `🎭 Bind Roles` button alongside `⚙️ Bind Speakers`
- **Setup UI — "Add Speaker" sub-page**: `➕ Add Speaker` moved to the main menu and now updates the message in-place showing the OAuth invite link and a `🏠 Main Menu` return button, keeping navigation consistent
- **Speaker gateway watchdog**: `pool.Service.StartWatchdog` runs every 30 s; logs a warning for any disconnected gateway (disgo's internal backoff handles reconnection) and actively reconnects bots whose gateway never connected at startup
- **Gateway reconnection on member join**: `TrySeedMember` and `SeedExistingSpeakers` attempt `pool.Reconnect` before giving up when `newSpeaker` fails due to a missing pool client
- **Recovery for unregistered guild members**: `NextSpeakerID` now calls `TrySeedMember` in the background for any pool bot that is already a guild member but missing from the speakers map (e.g. seeding failed at startup)
- **Caller role check on voice join**: `onVoiceJoin` logs whether the joining user holds the configured capture role (`allowedToSpeak`)
- **Hard startup validation**: `bot.New` returns an error if any speaker gateway fails to connect; owner gateway failure in `bot.Run` is surfaced as an error to `main`

### Changed
- `pool.ConnectPool` stores a `*bot.Client` for every valid-token bot even when `OpenGateway` fails, preserving the token for later reconnection
- `GetClientByID` now returns `false` for bots whose gateway is not in `StatusReady` (was: any stored client)

## [0.2.0] - 2026-03-28

### Added
- Owner bot invite URL is now logged at startup automatically
- Architecture diagram (Mermaid) added to README and discord app setup docs

### Breaking Changes
- Speaker token env var renamed from `DISCORD_SPEAKER_N_BOT_TOKEN` to `DISCORD_SPEAKER_BOT_TOKEN_N`; all env vars are now scanned via regex so gaps in numbering are fully supported

### Other changes
- Guild IDs for slash command sync are now sourced from the `Ready` event instead of polling the cache; falls back to global registration after a 10s timeout
- Background cleanup goroutine in `groupedCache` with a `Stop()` method for graceful shutdown
- `strconv.Atoi` errors in `handleSpeakersPage`, `handleToggleSpeaker`, and `handleBindChannel` are now logged instead of silently ignored
- `ownerClient.Caches.SelfUser()` return value checked in all call sites inside `manager.Service` — prevents a nil ID being used when the cache is not yet populated

## [0.1.0] - 2025-03-25

### Added

#### 🔊 Voice Relay Engine
- Owner bot listens in designated voice channel, capturing audio from members with configured capture role
- Speaker bots (unlimited) join bound voice channels and playback relayed audio in real time
- End-to-end encrypted audio via Discord's DAVE E2EE protocol through disgo and godave/libdave

#### 🤖 Speaker Pool
- Speaker bots loaded from sequential `DISCORD_SPEAKER_BOT_TOKEN_N` environment variables at startup
- All speaker gateways pre-connect concurrently for minimal join latency
- Speakers automatically registered on guild join, removed on guild leave

#### ⚙️ Interactive Setup UI
- `/setup` — paginated slash-command with Discord component menus for capture role, manager role, owner channel, and speaker binding
- `/bind-role` — directly set capture role
- `/bind-manager-role` — directly set manager role

#### 🚀 Voice Raid Management
- `/start` — caller bot joins channel; enabled speakers join theirs; relay begins
- `/stop` — speakers leave; relay stops; session torn down cleanly
- `/status` — ephemeral summary of bindings and session state

#### 🔐 Role-Based Access Control
- Administrator permission or manager role required for `/setup`, `/bind-role`, `/bind-manager-role`
- Manage Server permission or manager role required for `/start` and `/stop`

#### 💾 Persistent State
- All per-guild bindings persisted to YAML file (`store.yaml` by default)
- Thread-safe read/write; configurable via `STORE_PATH` environment variable

#### 🐳 Docker Support
- Multi-stage Dockerfile installs libdave, compiles with CGO
- CI pipeline builds on every push/PR; tagged images pushed to Docker Hub on version tags
