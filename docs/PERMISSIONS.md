# Bot permissions — go-discord-caller

go-discord-caller runs two kinds of bots, and they ask for different permissions:

- the **owner bot** captures voice from members with the capture role, answers
  slash commands, and manages the speaker bots;
- **speaker bots** join voice channels and play the relayed audio.

Both only work in voice. Every reply to a slash command goes through Discord's
interaction API, which needs no channel permission, so neither bot needs to
post in text channels.

## At a glance

| Permission | API flag | Bit | Value | Owner | Speaker | Used for |
|---|---|--:|--:|:---:|:---:|---|
| View Channel | `VIEW_CHANNEL` | 10 | `1024` | ✓ | ✓ | Seeing the voice channel it is bound to |
| Connect | `CONNECT` | 20 | `1048576` | ✓ | ✓ | Joining voice |
| Speak | `SPEAK` | 21 | `2097152` | ✓ | ✓ | Playing audio (the owner bot plays it in a guest server) |
| Use Voice Activity | `USE_VAD` | 25 | `33554432` | ✓ | ✓ | Transmitting in channels that require push-to-talk |
| Deafen Members | `DEAFEN_MEMBERS` | 23 | `8388608` | ✓ | | Server-deafening speaker bots that do not need to hear |
| Add Reactions | `ADD_REACTIONS` | 6 | `64` | ✓ | | Not used today; requested for upcoming features |
| Priority Speaker | `PRIORITY_SPEAKER` | 8 | `256` | ✓ | ✓ | Not used today |
| Send Messages | `SEND_MESSAGES` | 11 | `2048` | ✓ | | Not used today |
| Send TTS Messages | `SEND_TTS_MESSAGES` | 12 | `4096` | ✓ | | Not used today |
| Send Voice Messages | `SEND_VOICE_MESSAGES` | 46 | `70368744177664` | ✓ | | Not used today |
| Set Voice Channel Status | `SET_VOICE_CHANNEL_STATUS` | 48 | `281474976710656` | ✓ | | Not used today |

Each bot's permission integer is the sum of the values in its column:

| Bot | `permissions=` |
|---|---|
| Owner | `351843765984576` |
| Speaker | `36701440` |

## Owner bot

Invite it with:

```text
https://discord.com/oauth2/authorize?client_id=<client_id>&scope=bot&permissions=351843765984576
```

The same URL is printed to the logs when the bot starts (`owner bot invite URL`).

### Deafen Members

Discord forwards every speaker's voice to every bot in the channel, and each
bot decrypts all of it before it can be thrown away. A speaker bot that only
plays audio has no use for what it hears, so the owner bot server-deafens it,
and Discord stops sending it voice. This is the bot's biggest CPU saving. A
speaker bot that has to capture is undeafened as soon as it needs to hear.

Discord checks two things before it allows the deafen:

1. the owner bot has **Deafen Members**;
2. the owner bot's role is **above** every speaker bot's role in
   *Server Settings → Roles*.

Discord grants the permission at install time, but nobody can set the role
order from an invite link, so a server admin has to drag the owner bot's role
above the speaker bots' roles. Without both, the raid still runs: the deafen is
skipped with a warning, and the speaker bots keep decrypting audio they discard.

## Speaker bots

You do not invite speaker bots by hand. `/setup` → **Add Speaker** gives an
invite link for the next free speaker bot, scoped to your server and already
carrying the speaker permissions.

Speaker bots only join voice and play audio. Anything that acts on the server,
such as deafening members or posting messages, is done by the owner bot, so
speakers get the voice permissions and nothing else.

## Private voice channels

The permissions above are granted to the bot's role for the whole server. A
voice channel that denies **View Channel**, **Connect** or **Speak** to that
role, or to `@everyone` without allowing the bot back in, overrides them.
`/start` then refuses to begin the raid and lists each bot that cannot use its
channel. Add a permission override for the bot on that channel.

## Adding the bot from the App Directory

The **Add App** button on the App Directory and on the bot's profile ignores
the invite URL above. It uses the app's own install settings. In the
[Developer Portal](https://discord.com/developers/applications), open the owner
bot's app → **Installation**:

1. **Installation Contexts**: enable **Guild Install**.
2. **Install Link**: choose **Discord Provided Link**.
3. **Default Install Settings → Guild Install**: scopes `bot` and
   `applications.commands`, and tick the owner permissions from the table above.
4. Save, and check that the link the portal shows ends in
   `permissions=351843765984576`.

## Changing the permission set

A role keeps the permissions it was installed with. Servers that added the bots
before a change keep their old set; only new installs get the new one. To give
an existing server a newly required permission, re-open the invite URL, or have
an admin edit the bot's role.
