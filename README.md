# go-discord-caller

A Go Discord bot that captures voice audio from users with a specific role and relays it live to every bound speaker bot in a voice channel.

[![Hub](https://badgen.net/docker/pulls/sealbro/go-discord-caller?icon=docker&label=go-discord-caller](https://hub.docker.com/r/sealbro/go-discord-caller/)

## How it works

The system uses **two types of Discord bots**:

| Role                   | Description                                                                                                                                               |
|------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Owner / Caller bot** | The main bot. It listens in a voice channel, captures audio from users that have the configured capture role, and fans the audio out to all speaker bots. |
| **Speaker bots**       | A pool of secondary bots. Each speaker joins its own bound voice channel and plays back the audio relayed by the owner bot.                               |

All speaker gateways are pre-connected at startup. When a voice raid is started, the owner bot joins its channel, every enabled speaker joins theirs, and audio is streamed in real time via [disgo](https://github.com/disgoorg/disgo) + [godave / libdave](https://github.com/disgoorg/godave) (Discord's DAVE E2EE audio protocol).

## Features

- **Multi-speaker relay** – unlimited speaker bots, each bound to a different voice channel
- **Per-guild configuration** – capture role, manager role, owner channel, and per-speaker bindings are stored per guild
- **Interactive setup UI** – paginated slash-command menus with dropdowns and toggle buttons; no manual config file needed
- **Role-based access control** – a dedicated manager role can be configured to control who can start/stop raids without granting full admin
- **Auto-seeding** – speaker bots already in a guild are automatically registered on startup or when they join later
- **Graceful shutdown** – stops all active voice sessions and closes all gateways cleanly on `SIGTERM` / `Ctrl+C`
- **Docker-ready** – multi-stage Dockerfile produces a minimal distroless image with all shared libs bundled

## Slash commands

> Permissions are enforced at **runtime**. Users with the guild's configured manager role always satisfy the required permission check.

| Command             | Permission           | Description                                                                                                  |
|---------------------|----------------------|--------------------------------------------------------------------------------------------------------------|
| `/setup`            | Administrator        | Open the interactive setup panel (capture role, manager role, owner-channel picker, speaker binder)          |
| `/bind-role`        | Administrator        | Directly set the role whose members' voice will be captured and relayed                                      |
| `/bind-manager-role`| Administrator        | Set the role whose members are allowed to use `/setup`, `/start`, and `/stop`                                |
| `/start`            | Manage Server        | Make all enabled speakers join their bound voice channels and begin the voice raid                           |
| `/stop`             | Manage Server        | Stop the active voice raid and make all speakers leave their channels                                         |
| `/status`           | Everyone             | Show the current capture role, manager role, owner channel, speaker bindings, and session state              |

## Configuration

Configuration is loaded from environment variables (a `.env` file in the working directory is also supported via [godotenv](https://github.com/joho/godotenv)).

| Variable                      | Required | Description                                                    |
|-------------------------------|----------|----------------------------------------------------------------|
| `DISCORD_OWNER_BOT_TOKEN`     | ✅        | Token for the owner / caller bot                               |
| `DISCORD_SPEAKER_1_BOT_TOKEN` | ⚠️       | Token for the first speaker bot                                |
| `DISCORD_SPEAKER_2_BOT_TOKEN` | ⚠️       | Token for the second speaker bot                               |
| `DISCORD_SPEAKER_N_BOT_TOKEN` | ⚠️       | … sequential indices; stops at the first missing one           |
| `STORE_PATH`                  | ❌        | Path to the YAML persistence file (default: `store.yaml`)      |

> At least one speaker token is strongly recommended; without any, voice relay will not work.

### Example `.env`

```env
DISCORD_OWNER_BOT_TOKEN=your-owner-bot-token
DISCORD_SPEAKER_1_BOT_TOKEN=your-speaker-1-token
DISCORD_SPEAKER_2_BOT_TOKEN=your-speaker-2-token
```

## Running locally

```bash
go run ./cmd/bot
```

> **Dependencies:** the bot links against [libdave](https://github.com/disgoorg/godave) (CGO). Make sure `libdave` is installed and `PKG_CONFIG_PATH` points to its `.pc` file. See the Dockerfile for a reference install script.

## Docker

```bash
# Build
docker build -t go-discord-caller .

# Run
docker run --env-file .env go-discord-caller
```

The multi-stage build installs `libdave`, compiles the binary with CGO, then copies the binary and all shared-library dependencies into a minimal `distroless/base` image.

## Project structure

```
cmd/bot/            – application entry point
internal/
  bot/              – disgo client wiring, slash-command handlers, event listeners
  config/           – environment-variable loader
  domain/           – core types: GuildStatus, Speaker, VoiceSession
  manager/          – orchestrates speaker lifecycle and voice raid sessions
  pool/             – manages pre-connected speaker gateway clients
  speaker/          – joins channels and streams audio frames
  opus/             – voice frame provider/receiver adapters
  store/            – YAML-backed persistent state store
```

## Tech stack

- [disgo](https://github.com/disgoorg/disgo) – Discord API & gateway client
- [godave / libdave](https://github.com/disgoorg/godave) – Discord DAVE E2EE voice protocol (CGO)
- [godotenv](https://github.com/joho/godotenv) – `.env` file loading
- Go 1.26+
