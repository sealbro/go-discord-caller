# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
