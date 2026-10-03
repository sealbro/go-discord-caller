---
title: "Terms of Service — go-discord-caller"
description: >-
  Terms for the hosted go-discord-caller Discord bot: acceptable use, the
  consent expected before relaying voice, availability of the demo instance, and
  the absence of warranty.
---

# Terms of Service

**Last updated: 1 October 2026**

These terms apply to the **hosted demo instance** of go-discord-caller — the bot
you invite from this site. The source code is separate: it is licensed under the
[Apache License 2.0](https://github.com/sealbro/go-discord-caller/blob/main/LICENSE),
and running your own instance is governed by that licence, not by this page.

By inviting the bot to a Discord server, you agree to these terms on behalf of
that server.

## What the service is

A voice relay. It captures audio from users holding a configured role in one
voice channel and plays it back, live, in other voice channels — including
channels in other Discord servers when their administrators have joined the same
relay session.

The hosted instance exists **for evaluation**. It has a limited number of speaker
bots, it is currently provided free of charge, and it carries no uptime
commitment. For production use, self-host with your own bot tokens.

## Cost, and what may change

The hosted instance is free to use today. It is run at the operator's own
expense, and relaying live audio costs real money — every additional server and
every concurrent session needs capacity.

The operator therefore reserves the right to change how the hosted instance is
offered: to introduce paid plans or optional paid support, to limit how many
servers, speakers or concurrent sessions a server may use, to prioritise paying
users for capacity and reliability, or to stop offering the hosted instance
altogether. Any such change applies from the point it is made; nothing here
creates an obligation to keep the current terms, and no refund or credit arises
from a change since nothing is being paid.

Where a change is material and you are using the service, the intent is to give
reasonable notice through this site and the project's repository — but no notice
period is guaranteed.

!!! note "This does not affect the software"
    The source code is and remains free and open source under the Apache License
    2.0. That licence cannot be revoked for a version already released, so
    self-hosting stays free of charge regardless of what happens to the hosted
    instance. Nothing in this section limits your rights under that licence.

## Who may configure it

Binding channels and roles requires Administrator permission in the server;
starting and stopping sessions requires the configured manager role. By using
those commands you confirm you are authorised to make that decision for the
server.

## Relaying other people's voices

This is the obligation that matters most, so it is stated plainly.

**You must not use this bot to relay, or cause to be relayed, the voice of
anyone who has not been told it is happening.** Before starting a session, make
sure the members of the affected channels know that a bot is present, that their
audio is being carried to other channels, and whether those channels are in
other servers. Where your local law requires consent for the processing of
voice, obtaining it is your responsibility, not the operator's.

The bot does not record audio, and it must not be used as a means of covert
listening. If you cannot tell your members what the bot is doing, do not start
the session.

## Acceptable use

You agree not to:

- use the bot in violation of the [Discord Terms of Service](https://discord.com/terms),
  [Community Guidelines](https://discord.com/guidelines) or Developer policies
- relay audio you have no right to transmit, including copyrighted material and
  the voices of people who have not been informed
- use the bot to harass, deceive, impersonate or surveil anyone
- attempt to overload, disrupt or gain unauthorised access to the hosted
  instance or the infrastructure it runs on
- use the hosted instance for a purpose that requires guaranteed availability

The operator may remove the bot from any server, or decline to serve any server,
at any time — in particular where these terms appear to be broken.

## Availability and changes

The hosted instance may be changed, interrupted, limited or discontinued at any
time without notice. Relay codes, bindings and sessions may be reset. Nothing on
this site is a commitment to keep the service running.

## No warranty

The service is provided **"as is", without warranty of any kind**, express or
implied, including fitness for a particular purpose and non-infringement. To the
fullest extent permitted by law, the operator is not liable for any damages
arising from use of the hosted instance — including lost, delayed, distorted or
misdirected audio, or audio reaching a channel you did not intend.

Nothing in these terms limits liability that cannot be limited under applicable
law, including liability for death or personal injury caused by negligence, or
for fraud.

## Privacy

What the bot does with audio and data is described in the
[Privacy Policy](privacy.md).

## Contact

Questions and complaints:
**[github.com/sealbro/go-discord-caller/issues](https://github.com/sealbro/go-discord-caller/issues)**

## Changes to these terms

Material changes will be reflected here with a new date above. Continuing to use
the hosted instance after a change means accepting it. The page's history is
public in the
[repository](https://github.com/sealbro/go-discord-caller/commits/main/misc/website/terms.md).
