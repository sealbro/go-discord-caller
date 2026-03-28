# go-discord-caller

A Go Discord bot that captures voice audio from a user with a specific role and relays it live to every bound speaker bot in a voice channel.

[![Hub](https://badgen.net/docker/pulls/sealbro/go-discord-caller?icon=docker&label=go-discord-caller)](https://hub.docker.com/r/sealbro/go-discord-caller/)

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

## Slash commands

> Manager role is configured via `/setup`. `/status` is available to everyone.

| Command   | Required role      | Description                                                                                         |
|-----------|--------------------|-----------------------------------------------------------------------------------------------------|
| `/setup`  | Manager role       | Open the interactive setup panel (capture role, manager role, owner-channel picker, speaker binder) |
| `/start`  | Manager role       | Make all enabled speakers join their bound voice channels and begin the voice raid                  |
| `/stop`   | Manager role       | Stop the active voice raid and make all speakers leave their channels                               |
| `/status` | Everyone           | Show the current capture role, manager role, owner channel, speaker bindings, and session state     |

## Configuration

Configuration is loaded from environment variables (a `.env` file in the working directory is also supported via [godotenv](https://github.com/joho/godotenv)).

| Variable                      | Required | Description                                                    |
|-------------------------------|----------|----------------------------------------------------------------|
| `DISCORD_OWNER_BOT_TOKEN`     | ✅        | Token for the owner / caller bot                               |
| `DISCORD_SPEAKER_BOT_TOKEN_1` | ⚠️       | Token for the first speaker bot                                |
| `DISCORD_SPEAKER_BOT_TOKEN_2` | ⚠️       | Token for the second speaker bot                               |
| `DISCORD_SPEAKER_BOT_TOKEN_N` | ⚠️       | … any numeric suffix; gaps in numbering are supported          |
| `STORE_PATH`                  | ❌        | Path to the YAML persistence file (default: `store.yaml`)      |

> At least one speaker token is strongly recommended; without any, voice relay will not work.

## Discord app setup

### 1. Create the bots

For each bot (owner first, then one per speaker) go to [https://discord.com/developers/applications](https://discord.com/developers/applications), click **New Application**, give it a name, set a profile image and banner.

Then open the **Bot** section:
- Click **Reset Token**, copy and save it — you won't see it again
- **Owner bot only:** enable **Server Members Intent** under *Privileged Gateway Intents*

Add all tokens to your `.env`:
```env
DISCORD_OWNER_BOT_TOKEN=your-owner-token
DISCORD_SPEAKER_BOT_TOKEN_1=your-speaker-1-token
DISCORD_SPEAKER_BOT_TOKEN_2=your-speaker-2-token
```

To invite the **owner bot** to your server, open the **Installation** section, copy the Install Link and append the required scope and permissions:
```
https://discord.com/oauth2/authorize?client_id=<client_id>&scope=bot&permissions=391565762894144
```
> The ready-to-use invite URL is also printed to the logs automatically when the bot starts (`owner bot invite URL`).

> Speaker bots do **not** need to be added to the server manually — use the `/setup` command after the bot is running to invite them one by one.

### 2. Start the bot and finish setup

1. Start the bot (see [Running locally](#running-locally) or [Docker](#docker)).
2. In your Discord server, run `/setup` to open the interactive panel:
   - Bind the **capture role** — members with this role will have their voice relayed
   - Bind the **manager role** — members with this role can use `/start` and `/stop`
   - Bind the **owner bot** to a voice channel
   - Add speaker bots via the **Add Speaker** button and bind each to a voice channel
3. Run `/start` to begin a voice raid.

---

## Running locally

```bash
go run ./cmd/bot
```

> **Dependencies:** the bot links against [libdave](https://github.com/disgoorg/godave) (CGO). Make sure `libdave` is installed and `PKG_CONFIG_PATH` points to its `.pc` file. See the Dockerfile for a reference install script.

## Docker

### Pull from Docker Hub

```bash
# Pull and run (recommended)
docker run -d \
  --env-file .env \
  -e STORE_PATH=/data/store.yaml \
  -v $(pwd)/data:/data \
  sealbro/go-discord-caller
```

The YAML store is mounted from `./data/store.yaml` on the host so bindings survive container restarts.

Individual env variables can be passed with `-e` instead of `--env-file`:

```bash
docker run -d \
  -e DISCORD_OWNER_BOT_TOKEN=your-owner-token \
  -e DISCORD_SPEAKER_BOT_TOKEN_1=your-speaker-1-token \
  -e STORE_PATH=/data/store.yaml \
  -v $(pwd)/data:/data \
  sealbro/go-discord-caller
```

### Build locally

```bash
docker build -t go-discord-caller .

docker run -d --env-file .env go-discord-caller
```

The multi-stage build installs `libdave`, compiles the binary with CGO, then copies the binary and all shared-library dependencies into a minimal `distroless/base` image.

## Tech stack

- [disgo](https://github.com/disgoorg/disgo) – Discord API & gateway client
- [godave / libdave](https://github.com/disgoorg/godave) – Discord DAVE E2EE voice protocol (CGO)
- [godotenv](https://github.com/joho/godotenv) – `.env` file loading
- Go 1.26+
