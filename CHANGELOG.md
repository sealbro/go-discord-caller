# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
