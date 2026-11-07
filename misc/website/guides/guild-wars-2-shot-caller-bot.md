---
title: "Guild Wars 2 Shot Caller Bot for Discord"
description: >-
  Relay your Guild Wars 2 shot caller's voice live to every squad channel on Discord. Free hosted bot or self-hosted; works across servers, with DAVE end-to-end encryption.
keywords: >-
  guild wars 2 voice bot, gw2 wvw discord shot caller, gw2 commander voice relay discord, guild wars 2 squad voice, discord shot caller bot, discord voice relay bot
---

# Guild Wars 2 Shot Caller Bot for Discord

**go-discord-caller** relays your shot caller's voice live to every squad voice channel, across one or many Discord servers. It is built for Guild Wars 2 WvW commanders and squads, and works the same for any game.

[:simple-discord: Add the hosted bot](https://discord.com/discovery/applications/1484911601210495038){ .md-button .md-button--primary }
[:material-github: Self-host it](https://github.com/sealbro/go-discord-caller){ .md-button }

*The hosted bot is free to use and has a limited number of speakers; at peak times, when load is high, capacity may be limited ([terms](../terms.md)). Self-hosting is free under the Elastic License 2.0 with unlimited speakers and servers.*

## The problem

In Guild Wars 2 World vs World, a commander leads several squads at once. Squads often sit in separate voice channels, and the commander's call has to reach all of them without everyone crowding one channel.

## What you get

- WvW: the commander speaks once, every squad channel hears it.
- Multiple guilds on one map: link their servers with a relay code.
- Squad leaders confer in conference mode.
- **No client software:** members use Discord only. No overlay and no extra app.
- **Encrypted audio:** DAVE end-to-end encrypted voice.
- **8 languages:** slash commands and setup UI.

## Set it up in a few minutes

1. [Add the hosted bot](https://discord.com/discovery/applications/1484911601210495038) to your server, or [self-host it](https://github.com/sealbro/go-discord-caller).
2. Run `/setup`: pick the role that may speak (the capture role) and the role that may start sessions.
3. Bind one speaker bot to each squad voice channel.
4. Join the command channel and run `/start`. Anyone with the capture role who speaks is heard in every bound squad channel.

Need more than one caller? Use `/start mode:many` for a conference where everyone hears everyone except themselves (mix-minus). Want squads to stay silent toward each other? Use `/start mode:one-many`. Details: [how it works](../voice-flow.md).

## Related

- [Free vs paid shot-caller bots](../free-shot-caller-bot.md)
- [TeamSpeak and Mumble alternative](teamspeak-alternative.md)
- [Can a Discord bot be in multiple voice channels?](bot-in-multiple-voice-channels.md)
- [Audio latency](../latency.md)
