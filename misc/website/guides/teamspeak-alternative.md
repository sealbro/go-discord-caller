---
title: "TeamSpeak and Mumble Alternative: Shot Calling on Discord"
description: >-
  Run shot calling inside Discord instead of a separate TeamSpeak or Mumble server.
  A voice relay bot sends one caller to many squad channels, across servers.
keywords: >-
  teamspeak alternative, mumble alternative, discord instead of teamspeak, discord shot caller bot,
  guild voice comms without teamspeak
---

# TeamSpeak and Mumble alternative for shot calling

Many guilds keep a separate TeamSpeak or Mumble server only because Discord makes it awkward to talk to many channels at once. With a relay bot you can keep calls inside Discord.

## What changes

| | Separate voice server | Discord with go-discord-caller |
|---|---|---|
| Extra app and login | Yes | No, members already use Discord |
| One voice to many channels | One big room | Caller speaks once, each squad channel hears it |
| Allied guilds | Share one server | Each guild keeps its own server; link with a relay code |
| Many callers | Everyone in one room | Conference mode with mix-minus (no echo) |
| Hosting | You run the server | Hosted bot, or self-host with Docker |

## Where a dedicated server is still better

Be realistic: if your community wants positional audio, a client-side overlay, or its own server rules, a dedicated voice server can still fit. The relay bot is for guilds that already use Discord and want one clear voice at the front.

## Set it up in a few minutes

1. [Add the hosted bot](https://discord.com/discovery/applications/1484911601210495038) to your server, or [self-host it](https://github.com/sealbro/go-discord-caller).
2. Run `/setup`: pick the role that may speak (the capture role) and the role that may start sessions.
3. Bind one speaker bot to each squad voice channel.
4. Join the command channel and run `/start`. Anyone with the capture role who speaks is heard in every bound squad channel.

Need more than one caller? Use `/start mode:many` for a conference where everyone hears everyone except themselves (mix-minus). Want squads to stay silent toward each other? Use `/start mode:one-many`. Details: [how it works](../voice-flow.md).

*The hosted bot is free to use and has a limited number of speakers; at peak times, when load is high, capacity may be limited ([terms](../terms.md)). Self-hosting is free under the Elastic License 2.0 with unlimited speakers and servers.*

[Add the hosted bot](https://discord.com/discovery/applications/1484911601210495038){ .md-button .md-button--primary }
