---
title: "Can a Discord Bot Be in Multiple Voice Channels?"
description: >-
  A Discord bot holds one voice connection per server, so reaching many channels
  takes one bot per channel. Here is how a speaker-bot pool relays one caller to many channels.
keywords: >-
  can a discord bot be in multiple voice channels, discord bot join multiple voice channels,
  discord bot voice limit, discord voice relay bot, discord broadcast voice to multiple channels
---

# Can a Discord bot be in multiple voice channels?

A bot can be in one voice channel per server at a time. It can be connected to voice in several different servers at once, but not in two channels of the same server.

## So how do you reach many channels?

Use one bot per channel. go-discord-caller keeps a **pool of speaker bots**: the owner bot listens in the command channel, and each speaker bot sits in one squad channel and plays the audio back.

1. The owner bot captures audio from members who hold the capture role.
2. The audio is relayed in real time to every bound speaker bot.
3. Each speaker bot plays it in its own channel.

For other servers, a relay code links the guilds so the same call reaches allied channels too.

## Can members talk back?

Yes. `/start mode:many` is a conference where everyone hears everyone except themselves (mix-minus), and `/start mode:one-many` lets the caller hear the squads while squads hear only the caller.

## Set it up in a few minutes

1. [Add the hosted bot](https://discord.com/oauth2/authorize?client_id=1484911601210495038&scope=bot&permissions=351843765984576) to your server, or [self-host it](https://github.com/sealbro/go-discord-caller).
2. Run `/setup`: pick the role that may speak (the capture role) and the role that may start sessions.
3. Bind one speaker bot to each squad voice channel.
4. Join the command channel and run `/start`. Anyone with the capture role who speaks is heard in every bound squad channel.

Need more than one caller? Use `/start mode:many` for a conference where everyone hears everyone except themselves (mix-minus). Want squads to stay silent toward each other? Use `/start mode:one-many`. Details: [how it works](../voice-flow.md).

*The hosted bot is free to use and has a limited number of speakers; at peak times, when load is high, capacity may be limited ([terms](../terms.md)). Self-hosting is free under the Elastic License 2.0 with unlimited speakers and servers.*

[How it works](../voice-flow.md){ .md-button }
