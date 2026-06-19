---
title: "Free Shot Caller Bot vs Paid Shot-Caller Services"
description: >-
  Why pay a monthly subscription for a Discord shot caller bot? go-discord-caller
  is a free, open-source, self-hosted alternative to paid shot-caller services —
  unlimited servers and speakers, full conference mode, and DAVE end-to-end
  encryption, with no plan tiers.
keywords: >-
  free shot caller bot, open source shot caller bot, self-hosted shot caller bot,
  paid shot caller alternative, discord shot caller bot, open source discord voice relay,
  discord voice coordination bot, free discord voice relay
---

# A Free Shot-Caller Bot vs Paid Shot-Caller Services

Most Discord shot-caller bots are paid, hosted services — a "speak once, reach every squad" workflow gated behind a monthly subscription, with tiers that cap how many servers and parties you can use. **go-discord-caller** gives you the same workflow for free: it's open source and self-hosted, so you run it with your own Discord bot tokens with no plan tiers, no party limits, and end-to-end encrypted audio.

[:material-discord: Try the hosted bot](https://discord.com/oauth2/authorize?client_id=1484911601210495038&scope=bot&permissions=391565762894144){ .md-button .md-button--primary }
[:material-github: Self-host it (GitHub)](https://github.com/sealbro/go-discord-caller){ .md-button }

## Comparison

| | go-discord-caller | Typical paid shot-caller service |
|---|---|---|
| **Price** | Free & open source (Apache-2.0) | Monthly subscription (tiered) |
| **Servers** | Unlimited (inter-guild relay) | Usually capped by plan |
| **Speaker channels / parties** | Unlimited (one speaker bot per channel) | Usually capped by plan |
| **Hosting** | Self-hosted — you own the data | Managed SaaS |
| **Caller modes** | One-caller, hub-and-spoke, full conference (mix-minus) | Typically one-way broadcast |
| **Encryption** | DAVE end-to-end encrypted voice | Varies / often unstated |
| **Localization** | 7 languages | Often English only |
| **Source code** | Open on GitHub | Closed |

> Capabilities of paid services vary by provider and plan; check each vendor for current details.

## Why teams switch from paid services

- **No subscription** — host it once and run as many relays as you want.
- **No server or party caps** — relay across any number of Discord servers with as many speaker channels as you bind.
- **Full conference mode** — not just one-way broadcast: everyone can hear everyone with mix-minus echo prevention (`/start mode:many`).
- **Privacy** — self-hosted with DAVE end-to-end encryption; your audio never sits on a third-party service.

## Is self-hosting hard?

No. It's a single Docker container:

```bash
docker run -d \
  -e DISCORD_OWNER_BOT_TOKEN=your-owner-token \
  -e DISCORD_SPEAKER_BOT_TOKEN_1=your-speaker-1-token \
  -e STORE_PATH=/data/store.yaml \
  -v $(pwd)/data:/data \
  sealbro/go-discord-caller
```

Create one owner bot and one or more speaker bots in the [Discord Developer Portal](https://discord.com/developers/applications), drop the tokens in, and run `/setup` in your server. Prefer not to host at all? Use the [hosted bot](https://discord.com/oauth2/authorize?client_id=1484911601210495038&scope=bot&permissions=391565762894144) for evaluation.

See the [full setup guide on GitHub](https://github.com/sealbro/go-discord-caller#discord-app-setup) and [how it works](https://github.com/sealbro/go-discord-caller/blob/main/docs/VOICE_FLOW.md).
