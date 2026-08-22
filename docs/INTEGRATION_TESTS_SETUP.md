# Integration Test Setup

This document covers everything needed to run the integration test suite in `integration/`.
Do this once per environment (local dev or CI). The actual tests run with:

```bash
go test --tags=integration --timeout=15m ./integration/...
```

---

## 1. Bot accounts you need

| Bot | Role | Notes |
|-----|------|-------|
| **owner bot** | existing production owner | same token as `DISCORD_OWNER_BOT_TOKEN`; keep permissions identical to production |
| **speaker bot(s)** | existing production speakers | same tokens as `DISCORD_SPEAKER_BOT_TOKEN_N` |
| **source bot** | harness audio source | new dedicated bot; must never be a production speaker |
| **source bot 2** | second harness source | required for E2, E5b, E5c, E5d, E5f, E6, E12, E13–E15 (mix-minus, star topology, auto-route, cross-guild caller) |
| **listener bot** | harness frame counter + **test-admin** | new dedicated bot; grant **Administrator** in the test guild only |

The **listener bot** doubles as the test-admin for privileged REST calls (E9: move
members, E12: manage roles). Giving Administrator to the listener bot — not the
owner bot — keeps the owner bot at production-identical permissions, so tests
cannot pass on permissions that don't exist in production.

Create source and listener bots at https://discord.com/developers/applications.
Enable the **bot** scope; no privileged intents needed beyond Voice.

---

## 2. Test guild setup (one-time, manual)

### 2.1 Host guild

Create or reuse a private server. Do **not** use a production guild.

**Voice channels** (any names, note the IDs):

| Channel | Purpose | Env var |
|---------|---------|---------|
| `#owner` | owner bot listens here | `E2E_OWNER_CHANNEL_ID` |
| `#speaker-1` | first speaker bot plays here | `E2E_SPEAKER_CHANNEL_ID` |
| `#speaker-2` | second speaker bot plays here | `E2E_SPEAKER2_CHANNEL_ID` (optional) |

**Roles** (create these, note the IDs):

| Role | Purpose | Env var |
|------|---------|---------|
| `caller` | grants audio capture permission | `E2E_CALLER_ROLE_ID` |
| `manager` | not strictly needed for integration tests but expected by the bot | — |

**Bot invitations** — invite all bots with `bot` + `applications.commands` scope.

| Bot | Required permissions |
|-----|---------------------|
| owner bot | `View Channels`, `Connect`, `Speak`, `Use Voice Activity` — same as production, nothing extra |
| speaker bots | same as owner bot |
| source bots | `View Channels`, `Connect`, `Speak`, `Use Voice Activity` |
| listener bot | **Administrator** (test guild only) — used as test-admin for E9 / E12 |

E9 and E12 skip automatically if the listener bot lacks the necessary permissions.

After inviting, run `/setup` or the equivalent commands to bind the owner and speaker channels within the bot's store (this wiring is re-done programmatically in each test, but the bots must be guild members for `SeedExistingSpeakers` to find them).

**Assign the `caller` role** to:
- source bot
- source bot 2 (if used)

### 2.2 Guest guild (E5 / E5b / E5c / E5d / E5e / E5f)

Required for the inter-guild relay tests `TestE5_InterGuildRelay`,
`TestE5b_AllyCaller`, `TestE5c_OneManyAllyCaller`, `TestE5d_RelayResumesAfterIdleHost`,
`TestE5e_RelayResumesAfterIdleGuest`, and `TestE5f_StarRelayResumesAfterIdleHost`.
Create a second private
server. (E5b, E5c, E5d and E5f additionally need source bot 2; E5c needs
`E2E_SPEAKER2_CHANNEL_ID` in the host guild.)

- Invite owner bot and at least one speaker bot.
- Create one owner voice channel (`E2E_GUEST_OWNER_CHANNEL_ID`) and one speaker voice channel (`E2E_GUEST_SPEAKER_CHANNEL_ID`).
- Invite the listener bot.
- Assign the `caller` role to source bot in this guild too.

---

## 3. Generate sample audio

The source bot discovers all `*.dca` files in the samples directory and plays
them in random order: when one file reaches the end it picks a different file at
random (never the same file twice in a row when multiple files exist). Generate
the files once and commit them so CI needs no TTS tooling at runtime.

### 3.1 Prerequisites

```bash
# macOS — uses the built-in say command (no extra install needed)
brew install ffmpeg

# Debian / Ubuntu
apt-get install espeak-ng ffmpeg
```

### 3.2 Generate

```bash
go run ./cmd/gen-samples
```

On macOS this writes one file per `say` voice:

```
integration/samples/alex.dca
integration/samples/daniel.dca
integration/samples/karen.dca
integration/samples/nico.dca
integration/samples/samantha.dca
integration/samples/victoria.dca
```

Each file is a few seconds of speech at 64 kbps Opus. Commit them — they are
binary but small.

To override the directory, set `E2E_SAMPLES_DIR` to any directory containing
`*.dca` files. Every file matching the glob will be played in random order
until the test ends.

---

## 4. Environment variables

Copy the block below into `.env.integration` at the repo root and fill in the values.
The file is loaded automatically by the harness (gitignored — never commit tokens).

```dotenv
# --- Production bots (same as your .env) ---
DISCORD_OWNER_BOT_TOKEN=
DISCORD_SPEAKER_BOT_TOKEN_1=
# DISCORD_SPEAKER_BOT_TOKEN_2=   # add more as needed

# --- Harness bots ---
E2E_SOURCE_BOT_TOKEN=
E2E_SOURCE_BOT_TOKEN_2=          # optional; required for E2 / E6
E2E_LISTENER_BOT_TOKEN=

# --- Host guild ---
E2E_TEST_GUILD_ID=
E2E_OWNER_CHANNEL_ID=
E2E_SPEAKER_CHANNEL_ID=
E2E_SPEAKER2_CHANNEL_ID=         # optional; required for E2 / E6
E2E_CALLER_ROLE_ID=

# --- Guest guild (optional; required for E5) ---
E2E_GUEST_GUILD_ID=
E2E_GUEST_OWNER_CHANNEL_ID=
E2E_GUEST_SPEAKER_CHANNEL_ID=

# --- Audio (optional; defaults to integration/samples, all *.dca played randomly) ---
# E2E_SAMPLES_DIR=integration/samples
```

How to find IDs: enable **Developer Mode** in Discord settings, then right-click
any server / channel / role → **Copy ID**.

---

## 5. Local run

libdave (CGO) must be installed. The easiest way is to run inside the existing
Docker image:

```bash
# Build the image
docker build -t go-discord-caller .

# Run inside the image, mounting your .env.integration
docker run --rm \
  --env-file .env.integration \
  -v "$(pwd)/integration/samples:/app/integration/samples" \
  go-discord-caller \
  go test --tags=integration --timeout=15m ./integration/...
```

If libdave is already installed locally:

```bash
go test --tags=integration --timeout=15m -v ./integration/...
```

Run a single test:

```bash
go test --tags=integration --timeout=30s -v -run TestE1_OneCaller ./integration/...
```

### 5.1 Long-running stress tests (separate `stress` build tag)

`integration/stress_test.go` carries `//go:build stress`, **not** `integration`,
so the normal suite never picks it up. These tests run for tens of minutes (the
audible "play all bots" harness runs ~50 min) and are meant for manual, by-ear
verification — run them explicitly with `--tags=stress`:

```bash
go test --tags=stress -run TestStress_AllBotsPlayAudio -v -timeout 60m ./integration/...
```

| Test | What it does | Needs |
|------|--------------|-------|
| `TestStress_AllBotsPlayAudio` | Connects all three harness bots to channels and plays audio for ~50 min for by-ear checks. Does **not** start the manager/raid — run the bot manually first. | source bot 2, `E2E_SPEAKER2_CHANNEL_ID` |
| `TestStress_OneManyStarTopologyLong` | Sustained `OneManyGuildCaller` star-topology relay over a long window. | source bot 2, `E2E_SPEAKER2_CHANNEL_ID` |
| `TestStress_GuildCallerMixMinusLong` | Sustained `GuildCaller` mix-minus throughput; asserts no per-second gap > 2 s. | source bot 2, `E2E_SPEAKER2_CHANNEL_ID` |

### 5.2 Coverage

Without `-coverpkg`, Go only instruments the `integration` package itself (harness
helpers, assert utilities) — not the `internal/` packages where all the real
logic lives. Always pass `-coverpkg=./internal/...` to measure what the tests
actually exercise:

```bash
go test --tags=integration --timeout=15m \
  -coverprofile=coverage.out \
  -coverpkg=./internal/... \
  ./integration/...
```

`./integration/...` — which tests to **run**  
`-coverpkg=./internal/...` — which packages to **measure**

View results:

```bash
# Terminal summary (per-package percentages)
go tool cover -func=coverage.out

# HTML report saved to file (open in browser or load into GoLand)
go tool cover -html=coverage.out -o coverage.html
open coverage.html   # macOS
```

GoLand: **Run → Show Code Coverage Data…** (`⌘⌥F6`) → select `coverage.out`
to highlight covered/uncovered lines directly in the editor.

---

## 6. CI (GitHub Actions)

The workflow lives at `.github/workflows/integration.yml`. It runs:

- **Scheduled**: daily at 06:00 UTC
- **Manual**: `workflow_dispatch` (run it once before merging the harness)

Required secrets (set in repo Settings → Secrets):

```
DISCORD_OWNER_BOT_TOKEN
DISCORD_SPEAKER_BOT_TOKEN_1
E2E_SOURCE_BOT_TOKEN
E2E_SOURCE_BOT_TOKEN_2          # optional
E2E_LISTENER_BOT_TOKEN
E2E_TEST_GUILD_ID
E2E_OWNER_CHANNEL_ID
E2E_SPEAKER_CHANNEL_ID
E2E_SPEAKER2_CHANNEL_ID         # optional
E2E_CALLER_ROLE_ID
E2E_GUEST_GUILD_ID              # optional
E2E_GUEST_OWNER_CHANNEL_ID      # optional
E2E_GUEST_SPEAKER_CHANNEL_ID    # optional
```

The workflow uses `concurrency: { group: integration-discord, cancel-in-progress: false }` so
concurrent runs queue rather than cancel — two runs fighting over the same voice
channels would corrupt each other's state.

---

## 7. Test file layout

Tests are split across these files inside `integration/`. All carry the
`integration` build tag except `stress_test.go`, which is tagged `stress`;
`main_test.go` is tagged `integration || stress` so both suites share the
harness.

| File | Build tag | Contains |
|------|-----------|---------|
| `main_test.go` | `integration` \|\| `stress` | `TestMain`, shared `h *Harness` |
| `host_test.go` | `integration` | E1, E2, E4, E6, E7 — host-guild audio relay tests |
| `guest_test.go` | `integration` | E5, E5b, E5c, E5d, E5e, E5f — inter-guild relay |
| `handlers_test.go` | `integration` | E8–E12 — Discord event handler tests |
| `auto_route_test.go` | `integration` | E13–E15 — auto-router copy↔mix transitions |
| `stress_test.go` | `stress` | long-running by-ear / soak tests (see §5.1) |

---

## 8. Test coverage map

| Test | File | Mode (host / guest) | What it proves | Extra bots / perms needed |
|------|------|---------------------|---------------|--------------------------|
| E1 | host | `OneCaller` | basic relay: source → speaker | — |
| E2 | host | `GuildCaller` | mix-minus routing (A hears B, not itself) | source bot 2 |
| E4 | host | `OneCaller` | stop + restart raid resumes audio (voice reconnect, no gateway kill) | — |
| E5 | guest | `OneCaller` / `AllyListener` | inter-guild relay: host audio reaches guest speaker channel | guest guild |
| E5b | guest | `GuildCaller` / `AllyCaller` | bidirectional cross-guild: guest captures audio into host relay mixer | guest guild, source bot 2 |
| E5c | guest | `OneManyGuildCaller` / `OneManyAllyCaller` | star-topology guest: host owner audio → guest speakers, guests feed back | guest guild, source bot 2, `E2E_SPEAKER2_CHANNEL_ID` |
| E5d | guest | `GuildCaller` / `AllyCaller` | issue #51: guest→host relay survives an idle lull past `DrainIdleTimeout` (relay-fed mixers must not be paused by the cascade or the drain watcher) | guest guild, source bot 2 |
| E5e | guest | `GuildCaller` / `AllyCaller` | issue #51: host→guest relay survives the same lull when the guest joined in caller mode | guest guild |
| E5f | guest | `OneManyGuildCaller` / `OneManyAllyCaller` | issue #51 in star topology: guest→host relay into the hub mixer survives the lull (the direction E5c does not assert) | guest guild, source bot 2 |
| E6 | host | `OneManyGuildCaller` | star topology (hub hears all) | source bot 2 |
| E7 | host | `OneCaller` | `RequestMembers` pre-fetch allows pre-joined source | — |
| E8 | handlers | `OneCaller` | `onVoiceLeave` → `ReconnectBotChannel` after voice disconnect | — |
| E9 | handlers | `OneCaller` | `onVoiceMove` → `OnBotVoiceMove` → `ReconnectBotChannel` after admin move | listener bot: Move Members |
| E10 | handlers | `OneCaller` | `onVoiceJoin` live-join: caller joins after raid start → captured | — |
| E11 | handlers | `OneCaller` | `onVoiceLeave`/`onVoiceJoin` → auto-pause/resume via voice events | — |
| E12 | handlers | `GuildCaller` | `onGuildMemberUpdate` → AllowFilter updated on role revoke | source bot 2, listener bot: Manage Roles |
| E13 | auto_route | `OneCaller` | auto-router promotes source copy→mix on 2nd caller, no catastrophic gap | source bot 2 |
| E14 | auto_route | `OneCaller` | auto-router demotes mix→copy when a caller leaves, no lost delivery | source bot 2 |
| E15 | auto_route | `OneCaller` | two callers in one channel: sustained mixer throughput, no eviction/panic | source bot 2 |

Tests that require optional bots, guilds, or permissions are skipped automatically
when the corresponding env vars are unset or the REST call fails.

---

## 9. Cleanup hygiene

Each test registers a `t.Cleanup` that:

1. Stops the source bot's playback and leaves its voice channel.
2. Stops the listener bot and leaves its voice channel.
3. Calls `mgr.StopVoiceRaid` for every guild the test touched.

The speaker pool (`h.Pool`) is shared across all tests and is only shut down
once in `TestMain` — individual test cleanups must **not** call `mgr.Shutdown`
or `h.Pool.Shutdown`, as that would kill the pool for every subsequent test.

If a test panics mid-run, the cleanup still fires via `t.Cleanup`. Before each
test, `NewManager` creates a fresh in-memory store and re-seeds the pool, so
stale speaker bindings from a previous test do not bleed over.

If a bot is still in a voice channel from a previous crashed run, `TestSpeaker.StartPlaying`
calls `Leave` before `Join` to avoid the stale-connection hang that occurs when
`VoiceManager.CreateConn` returns an existing partially-closed connection.
