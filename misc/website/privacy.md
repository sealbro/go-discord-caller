---
title: "Privacy Policy — go-discord-caller"
description: >-
  What the hosted go-discord-caller bot does with voice audio and Discord data:
  nothing is recorded, bindings are stored per server, and no user-level
  identifiers are exported in telemetry.
---

# Privacy Policy

**Last updated: 10 October 2026**

This policy covers the **hosted demo instance** of go-discord-caller — the bot
you invite from this site. If you run the software yourself, you are the
operator and this policy does not apply to your instance; see
[Self-hosting](#self-hosting) below.

go-discord-caller is a voice relay. It carries live audio from one voice channel
to others. Understanding one thing makes the rest of this policy easy to read:

!!! info "The bot is not a recorder"
    Voice audio is processed **in memory and in real time**, then discarded. It
    is never written to disk, never stored, and never sent anywhere except the
    Discord voice channels the server has bound as relay destinations.

## What the bot relays

While a session is running, audio from users holding the configured capture role
is decrypted, mixed and played back into the bound channels — which may include
channels **in other Discord servers**, if the server's administrators have joined
a relay session using a relay code.

If you are speaking in a channel a bot of this project has joined, assume you are
being relayed to the other channels in that session. Server administrators choose
those destinations; the bot has no way to inform every listener individually.
Use `/status` to see the current session's configuration, and leave the channel
if you do not wish to be relayed.

## What is stored

The bot keeps a small configuration file per Discord server. It contains:

- the server (guild) ID
- the speaker bots' user IDs, the voice channel bound to each, and whether each
  is enabled
- the role IDs chosen as the capture role and the manager role
- the server's relay code
- the server's pinned language, if one was set

That is the complete list. **No human user IDs, no usernames, no message
content, and no audio** are stored. The data describes the server's setup, not
its people, and it exists so that settings survive a restart.

It is kept for as long as the bot is in the server. Removing the bot, or asking
for the entry to be deleted, removes it.

## What leaves the host

**Discord.** The bot is a Discord application, so everything it does passes
through Discord's own systems under
[Discord's Privacy Policy](https://discord.com/privacy).

**Operational telemetry.** The hosted instance may export metrics, traces and
logs to a monitoring backend so the operator can see that it is working. These
carry server-level and bot-level identifiers only — guild ID and name, channel
ID, voice region, the bot accounts' own IDs and names, which command was used,
and counts of audio frames processed.

They deliberately carry **no identifier for any human user**. Where the software
needs to know whether audio problems affect one speaker or everybody, it counts
*how many* speakers fall into each category — all frames decrypted, some, or
none — and exports only those counts, never who they were. A test in the source
tree (`TestNoSenderIdentityIsExported`) exists to keep it that way.

**Logs** record operational events: which server a session started in, which bot
joined which channel, and errors. They name the project's own bot accounts, not
members of your server.

## Discord data the bot reads but does not keep

To do its job the bot reads, in memory only:

- which users are in a voice channel, so it knows who to capture and relay
- the roles of those users, to check them against the capture role
- the user ID of whoever runs a slash command, to check permissions

None of this is written to disk or exported.

## Your rights

If you are in the EU/EEA or UK, the GDPR gives you the right to access, correct,
export, restrict, object to, or erase personal data held about you.

In practice there is very little to exercise those rights against: the bot does
not store personal data about the members of a server. If you believe data about
you is held by the hosted instance, or you want a server's configuration entry
deleted, open an issue:

**[github.com/sealbro/go-discord-caller/issues](https://github.com/sealbro/go-discord-caller/issues)**

Please do not include personal information in a public issue beyond what is
needed to identify the server.

Requests are handled by the project maintainer, who acts as the data controller
for the hosted instance.

## Children

Discord requires its users to be at least 13, or older where local law demands
it. This bot is not directed at children and is not intended for anyone who is
not permitted to use Discord.

## Self-hosting

If you run your own instance, you control it, and you are the data controller for
whatever it processes. Two settings are worth knowing:

- `OTEL_ENDPOINT` decides whether telemetry is exported at all. Leave it empty
  and nothing leaves your host.
- `LOG_LEVEL` at `debug` emits additional diagnostic lines that include numeric
  Discord user IDs. The default, `info`, does not.

## Changes

Material changes to this policy will be reflected here with a new date above. The
page's history is public in the
[repository](https://github.com/sealbro/go-discord-caller/commits/main/misc/website/privacy.md).
