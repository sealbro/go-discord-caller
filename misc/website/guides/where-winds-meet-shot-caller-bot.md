---
title: "Where Winds Meet Shot Caller Bot for Discord"
description: >-
  Relay your Where Winds Meet shot caller's voice live to every squad channel on Discord. Free hosted bot or self-hosted; works across servers, with DAVE end-to-end encryption.
keywords: >-
  where winds meet voice bot, where winds meet shot caller discord, wwm guild war voice relay, where winds meet discord voice, discord shot caller bot, discord voice relay bot
---

# Where Winds Meet Shot Caller Bot for Discord

**go-discord-caller** relays your shot caller's voice live to every squad voice channel, across one or many Discord servers. It is built for Where Winds Meet guild wars, and works the same for any game.

[:simple-discord: Add the hosted bot](https://discord.com/oauth2/authorize?client_id=1484911601210495038&scope=bot&permissions=351843765984576){ .md-button .md-button--primary }
[:material-github: Self-host it](https://github.com/sealbro/go-discord-caller){ .md-button }

*The hosted bot is free to use and has a limited number of speakers; at peak times, when load is high, capacity may be limited ([terms](../terms.md)). Self-hosting is free under the Elastic License 2.0 with unlimited speakers and servers.*

## The problem

Guild wars in Where Winds Meet involve many members coordinating at once. When the guild splits across voice channels, the shot caller's voice needs to reach all of them live.

## What you get

- Guild wars: one caller, every channel hears the call.
- Allied guilds on separate servers share a relay code.
- Conference mode for team leads.
- **No client software:** members use Discord only. No overlay and no extra app.
- **Encrypted audio:** DAVE end-to-end encrypted voice.
- **8 languages:** slash commands and setup UI.

## Set it up in a few minutes

1. [Add the hosted bot](https://discord.com/oauth2/authorize?client_id=1484911601210495038&scope=bot&permissions=351843765984576) to your server, or [self-host it](https://github.com/sealbro/go-discord-caller).
2. Run `/setup`: pick the role that may speak (the capture role) and the role that may start sessions.
3. Bind one speaker bot to each squad voice channel.
4. Join the command channel and run `/start`. Anyone with the capture role who speaks is heard in every bound squad channel.

Need more than one caller? Use `/start mode:many` for a conference where everyone hears everyone except themselves (mix-minus). Want squads to stay silent toward each other? Use `/start mode:one-many`. Details: [how it works](../voice-flow.md).

## Related

- [Free vs paid shot-caller bots](../free-shot-caller-bot.md)
- [TeamSpeak and Mumble alternative](teamspeak-alternative.md)
- [Can a Discord bot be in multiple voice channels?](bot-in-multiple-voice-channels.md)
- [Audio latency](../latency.md)
