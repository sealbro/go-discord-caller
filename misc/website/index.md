---
title: "Free Discord Shot Caller & Voice Relay Bot (Open Source)"
description: >-
  Self-hosted Discord shot caller and voice relay bot in Go. One shot caller
  speaks and their audio is broadcast live to multiple speaker bots across one or
  many Discord servers, with mix-minus mixing and DAVE end-to-end encryption.
  Free and open source.
---

# Free, Self-Hosted Discord Shot Caller & Voice Relay Bot

**go-discord-caller** captures your **shot caller's** voice and **broadcasts it live to multiple speaker bots — across one or many Discord servers at once**. It supports single-caller broadcast, hub-and-spoke, and full multi-channel conference modes with mix-minus audio mixing and Discord's DAVE end-to-end encryption.

It is **100% free and open source** — you self-host it with your own bot tokens and own your data. No subscriptions, no per-server caps, no party limits.

[:material-discord: Try the hosted bot](https://discord.com/oauth2/authorize?client_id=1484911601210495038&scope=bot&permissions=391565762894144){ .md-button .md-button--primary }
[:material-github: View on GitHub](https://github.com/sealbro/go-discord-caller){ .md-button }
[Free vs paid shot-caller bots →](free-shot-caller-bot.md){ .md-button }

## Why use it

- **Free & open source (Apache-2.0)** — no monthly fee, no trial wall.
- **Unlimited servers & speakers** — inter-guild relay across any number of Discord servers; one speaker bot per voice channel, as many as you want.
- **Three caller modes** — one-caller broadcast, hub-and-spoke, or full conference with mix-minus echo prevention.
- **End-to-end encrypted audio** — uses Discord's DAVE E2EE voice protocol via [godave / libdave](https://github.com/disgoorg/godave).
- **Self-hosted & private** — runs in Docker; bindings persist in a YAML store you control.
- **Localized** — slash commands and setup UI in 7 languages (English, Español, Deutsch, Français, Português, Polski, Русский).

## How it works

One **owner / caller bot** listens in a voice channel and captures audio from users with a configured role, then fans it out to a pool of **speaker bots**, each playing back in its own channel — in the same server or in allied servers via a relay code.

```mermaid
graph TB
    subgraph Discord Guild
        CR["🎙️ Capture Role - 'caller'"]
        subgraph Owner Bot
            OB["🤖 Owner / Caller Bot"]
            OCH["🔊 Owner Voice Channel"]
        end
        subgraph Speaker Bots
            SP1["📢 Speaker Bot 1"]
            SP2["📢 Speaker Bot 2"]
            SP3["📢 Speaker Bot N"]
            SCH1["🔉 Speaker Channel 1"]
            SCH2["🔉 Speaker Channel 2"]
            SCH3["🔉 Speaker Channel N"]
        end
    end
    CR -- "🗣️ user with role speaks" --> OCH
    OB -- "👂 listens & captures audio" --> OCH
    OB -- "📡 relays audio frames" --> SP1
    OB -- "📡 relays audio frames" --> SP2
    OB -- "📡 relays audio frames" --> SP3
    SP1 -- "▶️ plays back" --> SCH1
    SP2 -- "▶️ plays back" --> SCH2
    SP3 -- "▶️ plays back" --> SCH3
```

See the [detailed voice flow](https://github.com/sealbro/go-discord-caller/blob/main/docs/VOICE_FLOW.md) and [end-to-end latency breakdown](https://github.com/sealbro/go-discord-caller/blob/main/docs/LATENCY.md).

## Caller modes

Who hears who depends on the mode (🟢 solid = can hear, 🔇 red dashed = cannot hear).

```mermaid
flowchart TB
    subgraph M1["1️⃣ One caller — /start mode:one"]
        direction LR
        O1["🎙️ Owner"] -->|"🟢 hears"| A1["🔊 Speaker 1"]
        O1 -->|"🟢 hears"| B1["🔊 Speaker 2"]
        A1 -.->|"🔇 can't"| O1
        B1 -.->|"🔇 can't"| O1
        A1 -.->|"🔇 can't"| B1
    end

    subgraph M2["2️⃣ Many callers — /start mode:many"]
        direction LR
        O2["🎙️ Owner"] <-->|"🟢"| A2["🔊 Speaker 1"]
        O2 <-->|"🟢"| B2["🔊 Speaker 2"]
        A2 <-->|"🟢"| B2
    end

    subgraph M3["3️⃣ One ↔ many / star — /start mode:one-many"]
        direction LR
        O3["🎙️ Owner ⭐ hub"] -->|"🟢 owner → all"| A3["🔊 Speaker 1"]
        O3 -->|"🟢 owner → all"| B3["🔊 Speaker 2"]
        A3 -->|"🟢 → owner"| O3
        B3 -->|"🟢 → owner"| O3
        A3 -.->|"🔇 can't"| B3
        B3 -.->|"🔇 can't"| A3
    end

    M1 ~~~ M2
    M2 ~~~ M3

    linkStyle 0,1,5,6,7,8,9,10,11 stroke:#22c55e,stroke-width:3px
    linkStyle 2,3,4,12,13 stroke:#ef4444,stroke-width:1.5px,stroke-dasharray:4
```

- **1️⃣ One caller** — the owner talks, speakers only listen and can't talk back.
- **2️⃣ Many callers** — full conference: everyone hears everyone (mix-minus echo prevention).
- **3️⃣ One ↔ many / star** — the owner is the hub: it hears all speakers, but each speaker only hears the owner, not each other.

## Quick start (Docker)

```bash
docker run -d \
  -e DISCORD_OWNER_BOT_TOKEN=your-owner-token \
  -e DISCORD_SPEAKER_BOT_TOKEN_1=your-speaker-1-token \
  -e STORE_PATH=/data/store.yaml \
  -v $(pwd)/data:/data \
  sealbro/go-discord-caller
```

Then run `/setup` in your server to bind the capture role, manager role, owner channel, and speaker bots — no config files needed. Full instructions are in the [README on GitHub](https://github.com/sealbro/go-discord-caller#discord-app-setup).

## Built for gaming guilds

Your shot caller speaks once and every squad hears the call instantly — no repeating yourself across channels. It's tactical voice communication and multi-squad coordination for **PvP battles, guild wars, sieges, node wars, and competitive raids**, with cross-server voice relay to unite allied guilds under a single command.

## Works with your favorite games

A game-agnostic Discord voice command system — it works for any guild on any title, including **Throne and Liberty, World of Warcraft, Guild Wars 2, Black Desert Online, Albion Online, New World, and EVE Online**.

## Use cases

- **Gaming guilds & alliances** — a shot caller's voice reaches every squad channel across allied servers during raids, sieges, node wars, and PvP.
- **Events & town halls** — broadcast one speaker to many listening rooms.
- **Cross-community relays** — link two or more Discord servers under a single voice command with a shared relay code.

## Learn more

- [How it works — voice flow diagrams](https://github.com/sealbro/go-discord-caller/blob/main/docs/VOICE_FLOW.md)
- [Free shot-caller bot vs paid services](free-shot-caller-bot.md)
- [Building a Discord Caller (Voice Relay) Bot in Go](https://dev.to/sealbro/building-a-discord-caller-voice-relay-bot-in-go-5b9h)
- [Source code on GitHub](https://github.com/sealbro/go-discord-caller)
