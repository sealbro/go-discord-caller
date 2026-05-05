# E2E Test Setup

This document covers everything needed to run the E2E test suite in `e2e/`.
Do this once per environment (local dev or CI). The actual tests run with:

```bash
go test --tags=e2e --timeout=15m ./e2e/...
```

---

## 1. Bot accounts you need

| Bot | Role | Notes |
|-----|------|-------|
| **owner bot** | existing production owner | same token as `DISCORD_OWNER_BOT_TOKEN` |
| **speaker bot(s)** | existing production speakers | same tokens as `DISCORD_SPEAKER_BOT_TOKEN_N` |
| **source bot** | harness audio source | new dedicated bot; must never be a production speaker |
| **source bot 2** | second harness source | required only for E2 (mix-minus) and E6 (star topology) |
| **listener bot** | harness frame counter | new dedicated bot |

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
| `manager` | not strictly needed for E2E but expected by the bot | — |

**Bot invitations** — invite all bots with `bot` + `applications.commands` scope and the following permissions: `View Channels`, `Connect`, `Speak`, `Use Voice Activity`.

After inviting, run `/setup` or the equivalent commands to bind the owner and speaker channels within the bot's store (this wiring is re-done programmatically in each test, but the bots must be guild members for `SeedExistingSpeakers` to find them).

**Assign the `caller` role** to:
- source bot
- source bot 2 (if used)

### 2.2 Guest guild (E5 only)

Required only for `TestE5_InterGuildRelay`. Create a second private server.

- Invite owner bot and at least one speaker bot.
- Create one owner voice channel (`E2E_GUEST_OWNER_CHANNEL_ID`) and one speaker voice channel (`E2E_GUEST_SPEAKER_CHANNEL_ID`).
- Invite the listener bot.
- Assign the `caller` role to source bot in this guild too.

---

## 3. Generate sample audio

The source bot discovers all `*.dca` files in the samples directory and plays
them round-robin: when one file reaches the end it advances to the next,
wrapping back to the first after the last. Generate the files once and commit
them so CI needs no TTS tooling at runtime.

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
e2e/samples/alex.dca
e2e/samples/daniel.dca
e2e/samples/karen.dca
e2e/samples/samantha.dca
e2e/samples/victoria.dca
```

Each file is a few seconds of speech at 64 kbps Opus. Commit them — they are
binary but small.

To override the directory, set `E2E_SAMPLES_DIR` to any directory containing
`*.dca` files. Every file matching the glob will be played in alphabetical order.

---

## 4. Environment variables

Copy the block below into `.env.e2e` at the repo root and fill in the values.
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

# --- Audio (optional; defaults to e2e/samples, all *.dca played round-robin) ---
# E2E_SAMPLES_DIR=e2e/samples
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

# Run E2E inside the image, mounting your .env.e2e
docker run --rm \
  --env-file .env.e2e \
  -v "$(pwd)/e2e/samples:/app/e2e/samples" \
  go-discord-caller \
  go test --tags=e2e --timeout=15m ./e2e/...
```

If libdave is already installed locally:

```bash
go test --tags=e2e --timeout=15m -v ./e2e/...
```

Run a single test:

```bash
go test --tags=e2e --timeout=30s -v -run TestE1_OneCaller ./e2e/...
```

---

## 6. CI (GitHub Actions)

The workflow lives at `.github/workflows/e2e.yml`. It runs:

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

The workflow uses `concurrency: { group: e2e-discord, cancel-in-progress: false }` so
concurrent runs queue rather than cancel — two runs fighting over the same voice
channels would corrupt each other's state.

---

## 7. Test coverage map

| Test | Mode | What it proves | Extra bots needed |
|------|------|---------------|-------------------|
| E1 | `OneCaller` | basic relay: source → speaker | — |
| E2 | `GuildCaller` | mix-minus routing (A hears B, not itself) | source bot 2 |
| E3 | `GuildCaller` | pause / resume state machine | — |
| E4 | `OneCaller` | speaker gateway kill + audio resume | — |
| E5 | `OneCaller` + guest | inter-guild relay | guest guild |
| E6 | `OneManyGuildCaller` | star topology (hub hears all) | source bot 2 |
| E7 | `OneCaller` | `RequestMembers` pre-fetch allows pre-joined source | — |

Tests that require optional bots or guilds are skipped automatically when the
corresponding env vars are unset.

---

## 8. Cleanup hygiene

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

If a bot is still in a voice channel from a previous crashed run, `SourceBot.StartPlaying`
calls `Leave` before `Join` to avoid the stale-connection hang that occurs when
`VoiceManager.CreateConn` returns an existing partially-closed connection.
