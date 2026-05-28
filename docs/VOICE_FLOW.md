# Voice Session Flow

State as of v0.8.1: inline fanout decode in `VoiceReceiver.dispatchFanout`, pull-based `SourceBuffer` mixer inputs, sink-callback mixer output, `DrainWatcher` auto-pause.

---

## RaidModeOneCaller — direct passthrough (no mixer pipeline)

Only the owner bot has a `VoiceReceiver` and it uses the **legacy bytes-channel path** (no `FanoutHandle` installed). Speakers are `VoiceProvider`-only.
Single source → **entire mixer pipeline is bypassed**. Raw Opus bytes flow from the owner `chIn` to all speaker `chOut`s and `ally.Session` via `startFanoutDirect`.
No PCM decode, no `Mixer`, no re-encode. `chOwnerOut` is not created (owner does not play back audio into its own channel).

```mermaid
flowchart TD
    subgraph HOST_GUILD["Host Guild"]
        subgraph ChA["Discord Channel A (owner)"]
            OwnerVR["Owner VoiceReceiver<br/>(legacy chan mode)"]
        end
        subgraph ChB["Discord Channel B"]
            Spk1VP["Speaker1 VoiceProvider"]
        end
        subgraph ChC["Discord Channel C"]
            Spk2VP["Speaker2 VoiceProvider"]
        end

        chIn["chIn (owner capture)"]
        OwnerVR --> chIn

        Direct["startFanoutDirect goroutine<br/>no decode · no mixer · no encode"]
        chIn --> Direct

        chOutB["chOut (spk1)"]
        chOutC["chOut (spk2)"]

        Direct -- "raw Opus (per-target copy)" --> chOutB
        Direct -- "raw Opus (per-target copy)" --> chOutC

        chOutB --> Spk1VP
        chOutC --> Spk2VP
    end

    subgraph RELAY["Inter-Guild Relay"]
        AllySession["ally.Session.BroadcastFromGuild"]
        Direct -- "raw Opus" --> AllySession
    end

    subgraph GUEST_GUILD["Guest Guild (AllyListener)"]
        GuestOwnerVP["Guest Owner VoiceProvider"]
        GuestSpkVP["Guest Speaker VoiceProvider(s)"]
        AllySession --> GuestOwnerVP
        AllySession --> GuestSpkVP
    end

    %% Owner capture path — blue
    linkStyle 0 stroke:#0d47a1,stroke-width:2px
    linkStyle 1 stroke:#1565c0,stroke-width:2px

    %% Direct → Speaker B — green
    linkStyle 2 stroke:#1b5e20,stroke-width:2px
    linkStyle 5 stroke:#a5d6a7,stroke-width:2px

    %% Direct → Speaker C — orange
    linkStyle 3 stroke:#e65100,stroke-width:2px
    linkStyle 6 stroke:#ffcc80,stroke-width:2px

    %% Relay path — purple
    linkStyle 4 stroke:#4a148c,stroke-width:2px

    %% Guest delivery — red
    linkStyle 7 stroke:#b71c1c,stroke-width:2px
    linkStyle 8 stroke:#e57373,stroke-width:2px
```

---

## RaidModeGuildCaller — all channels capture and relay

Every channel has both a `VoiceReceiver` (capture, fanout mode) and a `VoiceProvider` (playback).
On each Opus packet `VoiceReceiver.dispatchFanout` runs **inline on disgo's UDP goroutine**: it decodes once, then calls `SourceBuffer.Feed` on every registered target (one buffer per source × destination mixer). Each `Mixer.tick` pulls one frame from each input via `SourceBuffer.Pull`. There is no longer a per-source decode goroutine and no intermediate capture channel for fanout-mode receivers.
Each `Mixer` receives audio from all sources **except its own channel** (mix-minus). The owner bot also gets `chOwnerOut` so it plays back the mixed audio of other channels.

```mermaid
flowchart TD
    subgraph HOST_GUILD["Host Guild"]
        subgraph ChA["Discord Channel A"]
            OwnerVR["Owner VoiceReceiver<br/>(fanout mode)"]
            OwnerVP["Owner VoiceProvider"]
        end
        subgraph ChB["Discord Channel B"]
            Spk1VR["Speaker1 VoiceReceiver<br/>(fanout mode)"]
            Spk1VP["Speaker1 VoiceProvider"]
        end
        subgraph ChC["Discord Channel C"]
            Spk2VR["Speaker2 VoiceReceiver<br/>(fanout mode)"]
            Spk2VP["Speaker2 VoiceProvider"]
        end

        MixA["Mixer A (mix-minus Ch A)<br/>+ DrainWatcher"]
        MixB["Mixer B (mix-minus Ch B)<br/>+ DrainWatcher"]
        MixC["Mixer C (mix-minus Ch C)<br/>+ DrainWatcher"]
        RelayMix["Relay Mixer (all sources)"]

        %% Owner source: dispatchFanout → SourceBuffer.Feed on MixB, MixC, RelayMix
        OwnerVR -- "Feed (Frame)" --> MixB
        OwnerVR -- "Feed (Frame)" --> MixC
        OwnerVR -- "Feed (Frame)" --> RelayMix

        %% Speaker1 source: → MixA, MixC, RelayMix (skip MixB)
        Spk1VR -- "Feed (Frame)" --> MixA
        Spk1VR -- "Feed (Frame)" --> MixC
        Spk1VR -- "Feed (Frame)" --> RelayMix

        %% Speaker2 source: → MixA, MixB, RelayMix (skip MixC)
        Spk2VR -- "Feed (Frame)" --> MixA
        Spk2VR -- "Feed (Frame)" --> MixB
        Spk2VR -- "Feed (Frame)" --> RelayMix

        chOwnerOut["chOwnerOut"]
        chOutB["chOut (spk1)"]
        chOutC["chOut (spk2)"]

        MixA -- "SetSink" --> chOwnerOut
        MixB -- "SetSink" --> chOutB
        MixC -- "SetSink" --> chOutC

        chOwnerOut --> OwnerVP
        chOutB --> Spk1VP
        chOutC --> Spk2VP
    end

    subgraph RELAY["Inter-Guild Relay"]
        AllySession["ally.Session.BroadcastFromGuild"]
        RelayMix -- "SetSink" --> AllySession
    end

    subgraph GUEST_GUILD["Guest Guild"]
        GuestOwnerVP["Guest Owner VoiceProvider"]
        GuestSpkVP["Guest Speaker VoiceProvider(s)"]
        AllySession --> GuestOwnerVP
        AllySession --> GuestSpkVP
    end

    %% Owner source edges (Ch A) — blue
    linkStyle 0 stroke:#1565c0,stroke-width:2px
    linkStyle 1 stroke:#1976d2,stroke-width:2px
    linkStyle 2 stroke:#4a148c,stroke-width:2px

    %% Speaker1 source edges (Ch B) — green
    linkStyle 3 stroke:#2e7d32,stroke-width:2px
    linkStyle 4 stroke:#388e3c,stroke-width:2px
    linkStyle 5 stroke:#6a1b9a,stroke-width:2px

    %% Speaker2 source edges (Ch C) — orange
    linkStyle 6 stroke:#ef6c00,stroke-width:2px
    linkStyle 7 stroke:#f57c00,stroke-width:2px
    linkStyle 8 stroke:#8e24aa,stroke-width:2px

    %% Mixer sinks — channel colour
    linkStyle 9  stroke:#42a5f5,stroke-width:2px
    linkStyle 10 stroke:#66bb6a,stroke-width:2px
    linkStyle 11 stroke:#ffa726,stroke-width:2px

    %% chOut → Provider
    linkStyle 12 stroke:#90caf9,stroke-width:2px
    linkStyle 13 stroke:#a5d6a7,stroke-width:2px
    linkStyle 14 stroke:#ffcc80,stroke-width:2px

    %% Relay sink + guest delivery
    linkStyle 15 stroke:#ba68c8,stroke-width:2px
    linkStyle 16 stroke:#b71c1c,stroke-width:2px
    linkStyle 17 stroke:#e57373,stroke-width:2px
```

Implementation note: each edge labelled `Feed (Frame)` represents one `SourceBuffer` instance registered with the target mixer via `Mixer.AddInput`. The buffer has capacity 3 (`audioSourceCap` in `internal/opus/source.go`); overflow drops the oldest frame at the producer side so the mixer is always within 60 ms of the live edge.

---

## RaidModeGuildCaller + RaidModeAllyCaller — bi-directional inter-guild relay

Both guilds have a full mix-minus graph. Both use `BroadcastFromGuild` so neither
hears its own audio echoed back. `registerRelayInputs` wires relay inputs into each
guild's channel mixers so incoming relay audio is mixed alongside local sources.

- **Host → Guest**: `Relay Mixer` (SetSink) → `BroadcastFromGuild(hostID)` → guest `relayOpusIn` chan → Bridge (decode once, Feed) → guest channel mixers
- **Guest → Host**: guest `Relay Mixer` (SetSink) → `BroadcastFromGuild(guestID)` → host `relayOpusIn` chan → Bridge (decode once, Feed) → host channel mixers

The relay bridge is the **only remaining decode goroutine** in the pipeline. It reads packets off `relayOpusIn` (a buffered chan because relay frames arrive from another guild rather than via inline `ReceiveOpusFrame`), decodes once per packet, and `Feed`s a `Frame` into one `SourceBuffer` per destination mixer.

```mermaid
flowchart TD
    subgraph HOST["Host Guild"]
        subgraph HChA["Channel A (owner)"]
            HOwnerVR["Owner VoiceReceiver"]
            HOwnerVP["Owner VoiceProvider"]
        end
        subgraph HChB["Channel B"]
            HSpkVR["Speaker VoiceReceiver"]
            HSpkVP["Speaker VoiceProvider"]
        end

        HMixA["Mixer A (mix-minus)<br/>+ DrainWatcher"]
        HMixB["Mixer B (mix-minus)<br/>+ DrainWatcher"]
        HRelayMix["Relay Mixer"]
        HchOwnerOut["chOwnerOut"]
        HchOut["chOut (spk)"]
        HRelayIn["relayOpusIn (host)"]
        HBridge["Bridge goroutine<br/>(decode once, Feed)"]

        HOwnerVR -- "Feed" --> HMixB
        HOwnerVR -- "Feed" --> HRelayMix
        HSpkVR   -- "Feed" --> HMixA
        HSpkVR   -- "Feed" --> HRelayMix

        HRelayIn --> HBridge
        HBridge -- "Feed (Frame A)" --> HMixA
        HBridge -- "Feed (Frame B)" --> HMixB

        HMixA -- "SetSink" --> HchOwnerOut --> HOwnerVP
        HMixB -- "SetSink" --> HchOut      --> HSpkVP
    end

    subgraph SESSION["ally.Session"]
        BFGHost["BroadcastFromGuild(hostID)"]
        BFGGuest["BroadcastFromGuild(guestID)"]
    end

    HRelayMix -- "SetSink" --> BFGHost

    subgraph GUEST["Guest Guild"]
        subgraph GChA["Channel A (owner)"]
            GOwnerVR["Owner VoiceReceiver"]
            GOwnerVP["Owner VoiceProvider"]
        end
        subgraph GChB["Channel B"]
            GSpkVR["Speaker VoiceReceiver"]
            GSpkVP["Speaker VoiceProvider"]
        end

        GMixA["Mixer A (mix-minus)<br/>+ DrainWatcher"]
        GMixB["Mixer B (mix-minus)<br/>+ DrainWatcher"]
        GRelayMix["Relay Mixer"]
        GchOwnerOut["chOwnerOut"]
        GchOut["chOut (spk)"]
        GRelayIn["relayOpusIn (guest)"]
        GBridge["Bridge goroutine"]

        GOwnerVR -- "Feed" --> GMixB
        GOwnerVR -- "Feed" --> GRelayMix
        GSpkVR   -- "Feed" --> GMixA
        GSpkVR   -- "Feed" --> GRelayMix

        GRelayIn --> GBridge
        GBridge -- "Feed (Frame A)" --> GMixA
        GBridge -- "Feed (Frame B)" --> GMixB

        GMixA -- "SetSink" --> GchOwnerOut --> GOwnerVP
        GMixB -- "SetSink" --> GchOut      --> GSpkVP
    end

    GRelayMix -- "SetSink" --> BFGGuest

    BFGHost  --> GRelayIn
    BFGGuest --> HRelayIn

    %% Host owner — blue
    linkStyle 0 stroke:#1565c0,stroke-width:2px
    linkStyle 1 stroke:#4a148c,stroke-width:2px

    %% Host speaker — green
    linkStyle 2 stroke:#2e7d32,stroke-width:2px
    linkStyle 3 stroke:#6a1b9a,stroke-width:2px

    %% Host bridge → mixers — teal
    linkStyle 4 stroke:#00838f,stroke-width:2px
    linkStyle 5 stroke:#0097a7,stroke-width:2px
    linkStyle 6 stroke:#00acc1,stroke-width:2px

    %% Host sinks
    linkStyle 7 stroke:#42a5f5,stroke-width:2px
    linkStyle 8 stroke:#90caf9,stroke-width:2px
    linkStyle 9 stroke:#66bb6a,stroke-width:2px
    linkStyle 10 stroke:#a5d6a7,stroke-width:2px

    %% Host relay out — purple
    linkStyle 11 stroke:#ba68c8,stroke-width:2px

    %% Guest owner — cyan
    linkStyle 12 stroke:#00838f,stroke-width:2px
    linkStyle 13 stroke:#8e24aa,stroke-width:2px

    %% Guest speaker — orange
    linkStyle 14 stroke:#ef6c00,stroke-width:2px
    linkStyle 15 stroke:#8e24aa,stroke-width:2px

    %% Guest bridge — gold
    linkStyle 16 stroke:#f9a825,stroke-width:2px
    linkStyle 17 stroke:#fbc02d,stroke-width:2px
    linkStyle 18 stroke:#fdd835,stroke-width:2px

    %% Guest sinks
    linkStyle 19 stroke:#26c6da,stroke-width:2px
    linkStyle 20 stroke:#80deea,stroke-width:2px
    linkStyle 21 stroke:#ffa726,stroke-width:2px
    linkStyle 22 stroke:#ffcc80,stroke-width:2px

    %% Guest relay out — purple
    linkStyle 23 stroke:#ce93d8,stroke-width:2px

    %% Cross-guild relay delivery
    linkStyle 24 stroke:#00695c,stroke-width:2px
    linkStyle 25 stroke:#f57f17,stroke-width:2px
```

---

## RaidModeOneManyGuildCaller — star topology (host)

Star topology: the owner is the central hub. Only **one** channel `Mixer` is created — for the owner's channel (the hub). The owner's `FanoutHandle` is installed by `installFanoutOwnerStar` with two kinds of targets: **OpusTargets** that receive raw Opus bytes (one per-target pooled copy) and go directly to each speaker `chOut`, and one **SourceTarget** (`SourceBuffer`) registered with the relay mixer so guests still hear the owner.
Speaker sources decode + `Feed` into the hub mixer (and into the relay mixer) just like in mix-minus mode. Speakers cannot hear each other — they only hear the owner.

```mermaid
flowchart TD
    subgraph HOST_GUILD["Host Guild"]
        subgraph ChA["Discord Channel A (owner = hub)"]
            OwnerVR["Owner VoiceReceiver<br/>(fanout, OpusTargets + SourceTarget)"]
            OwnerVP["Owner VoiceProvider"]
        end
        subgraph ChB["Discord Channel B"]
            Spk1VR["Speaker1 VoiceReceiver<br/>(fanout)"]
            Spk1VP["Speaker1 VoiceProvider"]
        end
        subgraph ChC["Discord Channel C"]
            Spk2VR["Speaker2 VoiceReceiver<br/>(fanout)"]
            Spk2VP["Speaker2 VoiceProvider"]
        end

        MixA["Hub Mixer A<br/>(all speakers mixed)<br/>+ DrainWatcher"]
        RelayMix["Relay Mixer"]

        chOwnerOut["chOwnerOut"]
        chOutB["chOut (spk1)"]
        chOutC["chOut (spk2)"]

        %% Owner: raw Opus to speaker chOuts (no re-encode) + decoded Feed to relay
        OwnerVR -- "raw Opus" --> chOutB
        OwnerVR -- "raw Opus" --> chOutC
        OwnerVR -- "Feed (Frame)" --> RelayMix

        %% Speaker B: hub mixer ONLY + relay
        Spk1VR -- "Feed" --> MixA
        Spk1VR -- "Feed" --> RelayMix

        %% Speaker C: hub mixer ONLY + relay
        Spk2VR -- "Feed" --> MixA
        Spk2VR -- "Feed" --> RelayMix

        %% Guest relay enters at the hub mixer only
        HRelayInA["relayOpusIn (host)"]
        HBridge["Bridge goroutine"]
        HRelayInA --> HBridge
        HBridge -- "Feed (Frame)" --> MixA

        MixA -- "SetSink" --> chOwnerOut

        chOwnerOut --> OwnerVP
        chOutB --> Spk1VP
        chOutC --> Spk2VP
    end

    subgraph RELAY["Inter-Guild Relay"]
        AllySession["ally.Session.BroadcastFromGuild"]
        RelayMix -- "SetSink" --> AllySession
    end

    subgraph GUEST_GUILD["Guest Guild"]
        GuestOwnerVP["Guest Owner VoiceProvider"]
        GuestSpkVP["Guest Speaker VoiceProvider(s)"]
        AllySession --> GuestOwnerVP
        AllySession --> GuestSpkVP
    end

    %% Owner edges — blue
    linkStyle 0 stroke:#1565c0,stroke-width:2px
    linkStyle 1 stroke:#1976d2,stroke-width:2px
    linkStyle 2 stroke:#4a148c,stroke-width:2px

    %% Speaker1 edges — green
    linkStyle 3 stroke:#2e7d32,stroke-width:2px
    linkStyle 4 stroke:#6a1b9a,stroke-width:2px

    %% Speaker2 edges — orange
    linkStyle 5 stroke:#ef6c00,stroke-width:2px
    linkStyle 6 stroke:#8e24aa,stroke-width:2px

    %% Guest-relay bridge → hub mixer — gold
    linkStyle 7 stroke:#f9a825,stroke-width:2px
    linkStyle 8 stroke:#fbc02d,stroke-width:2px

    %% Hub mixer sink → owner — blue
    linkStyle 9  stroke:#42a5f5,stroke-width:2px
    linkStyle 10 stroke:#90caf9,stroke-width:2px

    %% Direct chOut → speakers
    linkStyle 11 stroke:#a5d6a7,stroke-width:2px
    linkStyle 12 stroke:#ffcc80,stroke-width:2px

    %% Relay sink + guest delivery
    linkStyle 13 stroke:#ba68c8,stroke-width:2px
    linkStyle 14 stroke:#b71c1c,stroke-width:2px
    linkStyle 15 stroke:#e57373,stroke-width:2px
```

---

## RaidModeOneManyGuildCaller + RaidModeOneManyAllyCaller — star topology inter-guild

Both guilds use the star topology. The host owner is the central hub across guilds.

- **Host**: as in the previous diagram — speakers hear only the owner (via owner's `OpusTargets`); owner hears all local speakers (hub mixer) plus host-side bridge-decoded guest relay packets.
- **Guest**: all captures `Feed` only the relay mixer (no local cross-channel mixing). **Channel mixers are not created.** Host relay packets arrive on `relayOpusIn` and are written directly to speaker `chOut`s as raw Opus via `ally.Session.AddGuild` (no bridge, no mixer) — same delivery path as `AllyListener`.

```mermaid
flowchart TD
    subgraph HOST["Host Guild"]
        subgraph HChA["Channel A (owner = hub)"]
            HOwnerVR["Owner VoiceReceiver<br/>(OpusTargets + SourceTarget)"]
            HOwnerVP["Owner VoiceProvider"]
        end
        subgraph HChB["Channel B"]
            HSpkVR["Speaker VoiceReceiver"]
            HSpkVP["Speaker VoiceProvider"]
        end

        HMixA["Hub Mixer A<br/>+ DrainWatcher"]
        HRelayMix["Relay Mixer"]
        HchOwnerOut["chOwnerOut"]
        HchOut["chOut (spk)"]
        HRelayIn["relayOpusIn (host)"]
        HBridge["Bridge goroutine"]

        HOwnerVR -- "raw Opus" --> HchOut
        HOwnerVR -- "Feed (Frame)" --> HRelayMix

        HSpkVR -- "Feed" --> HMixA
        HSpkVR -- "Feed" --> HRelayMix

        HRelayIn --> HBridge
        HBridge -- "Feed (Frame)" --> HMixA

        HMixA -- "SetSink" --> HchOwnerOut --> HOwnerVP
        HchOut --> HSpkVP
    end

    subgraph SESSION["ally.Session"]
        BFGHost["BroadcastFromGuild(hostID)"]
        BFGGuest["BroadcastFromGuild(guestID)"]
    end

    HRelayMix -- "SetSink" --> BFGHost

    subgraph GUEST["Guest Guild"]
        subgraph GChA["Channel A (owner)"]
            GOwnerVR["Owner VoiceReceiver<br/>(fanout, SourceTarget only)"]
            GOwnerVP["Owner VoiceProvider"]
        end
        subgraph GChB["Channel B"]
            GSpkVR["Speaker VoiceReceiver"]
            GSpkVP["Speaker VoiceProvider"]
        end

        GRelayMix["Relay Mixer"]
        GchOwnerOut["chOwnerOut"]
        GchOut["chOut (spk)"]

        %% Guest star: ALL sources → relay ONLY (no channel mixers)
        GOwnerVR -- "Feed" --> GRelayMix
        GSpkVR   -- "Feed" --> GRelayMix

        GchOwnerOut --> GOwnerVP
        GchOut      --> GSpkVP
    end

    GRelayMix -- "SetSink" --> BFGGuest

    BFGHost  -- "raw Opus" --> GchOwnerOut
    BFGHost  -- "raw Opus" --> GchOut
    BFGGuest --> HRelayIn

    %% Host owner — blue
    linkStyle 0 stroke:#1565c0,stroke-width:2px
    linkStyle 1 stroke:#4a148c,stroke-width:2px

    %% Host speaker — green
    linkStyle 2 stroke:#2e7d32,stroke-width:2px
    linkStyle 3 stroke:#6a1b9a,stroke-width:2px

    %% Host bridge → hub — gold
    linkStyle 4 stroke:#f9a825,stroke-width:2px
    linkStyle 5 stroke:#fbc02d,stroke-width:2px

    %% Host sinks
    linkStyle 6  stroke:#42a5f5,stroke-width:2px
    linkStyle 7  stroke:#90caf9,stroke-width:2px
    linkStyle 8  stroke:#a5d6a7,stroke-width:2px

    %% Host relay out
    linkStyle 9 stroke:#ba68c8,stroke-width:2px

    %% Guest sources → relay only — teal
    linkStyle 10 stroke:#00695c,stroke-width:2px
    linkStyle 11 stroke:#26a69a,stroke-width:2px

    %% Guest output (from host relay, direct)
    linkStyle 12 stroke:#e57373,stroke-width:2px
    linkStyle 13 stroke:#f06292,stroke-width:2px

    %% Guest relay out — purple
    linkStyle 14 stroke:#ce93d8,stroke-width:2px

    %% Cross-guild delivery
    linkStyle 15 stroke:#00695c,stroke-width:2px
    linkStyle 16 stroke:#26a69a,stroke-width:2px
    linkStyle 17 stroke:#f57f17,stroke-width:2px
```

---

## Mix-minus rule

Each channel `Mixer[X]` receives `Feed` calls from **every source except sources originating in channel X**.
This prevents echo: users in channel X would otherwise hear their own audio played back to them. The exclusion is enforced in `wireFanout` (`voice_fanout.go:88`) by simply *not registering* a `SourceBuffer` for `src.channelID == dest.channelID`.

### Star topology exception (OneManyGuildCaller / OneManyAllyCaller)

In star mode the rule tightens further:
- Host: speakers do not get their own per-channel mixer; the owner's `FanoutHandle` writes raw Opus directly to each speaker `chOut` via `OpusTargets`. Speakers `Feed` only the hub mixer (and the relay mixer).
- Guest star: sources `Feed` the relay mixer only, and channel mixers are not created at all — host relay arrives as raw Opus bytes delivered directly to speaker `chOut`s via `ally.Session.AddGuild` (same as `AllyListener`).

---

## Data flow summary

| Stage              | Component                                                | Description                                                                                                                                       |
|--------------------|----------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------|
| Capture (fanout)   | `VoiceReceiver.dispatchFanout`                           | Inline on disgo's UDP goroutine: role filter → Opus decode (once) → `Feed` each registered `SourceBuffer` and copy raw Opus to each `OpusTarget`. |
| Capture (legacy)   | `VoiceReceiver` → `chIn`                                 | **OneCaller only**: no `FanoutHandle`; raw Opus bytes pushed into a buffered chan for `startFanoutDirect`.                                        |
| Direct fanout      | `startFanoutDirect` goroutine                            | **OneCaller only**: raw Opus forwarded to speaker `chOut`s + relay, zero decode/encode.                                                           |
| Owner star fanout  | `installFanoutOwnerStar`                                 | **OneMany host**: installs `OpusTargets` (raw → speaker chOuts) + one `SourceBuffer` (decoded → relay mixer) on the owner's `FanoutHandle`.       |
| Mixer input        | `SourceBuffer` (cap 3)                                   | Lock-protected ring; `Feed` evicts the oldest frame on overflow so the mixer is always within 60ms of the live edge.                              |
| Per-channel mix    | `Mixer.tick` (20ms timer, `startChannelMixers`)          | Pulls one `Frame` per input; single-source passthrough forwards `Frame.Opus` directly; multi-source mixes PCM and re-encodes.                     |
| Relay mix          | `Mixer.tick` (relay), `startRelayBroadcast`              | Mixes all sources; `SetSink` calls `ally.Session.BroadcastFromGuild` synchronously.                                                               |
| Relay bridge       | one goroutine per attached guild (`registerRelayInputs`) | Reads incoming Opus packets, decodes **once**, `Feed`s a `Frame` into one `SourceBuffer` per destination mixer.                                   |
| Guest delivery     | `ally.Session.BroadcastFromGuild`                        | One Opus channel per guest guild; direct to `chOut`s (Listener / OneManyAllyCaller) or to `relayOpusIn` for bridge → mixers (AllyCaller).         |
| Playback           | `VoiceProvider.ProvideOpusFrame`                         | Reads from `chOut`; recycles previous frame to pool; bleed-off drains one extra frame per call when queue depth > 3.                              |

---

## Backpressure: producer-side overflow

The pre-v0.8.1 design read frames from buffered channels inside `Mixer.tick` and triggered a "drain-to-latest" burst when more than N frames had accumulated. That worked but caused audible N×20ms gaps when a stall ended.

The current design moves overflow to the producer. Every mixer input is a `SourceBuffer` ring of capacity 3 (`internal/opus/source.go:10`); `SourceBuffer.Feed` evicts the oldest frame inline. The consumer never sees a backlog larger than 60ms and never needs a burst drain.

| Drain point       | Location                                       | Mechanism                                                                                                                  |
|-------------------|------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------|
| **Mixer inputs**  | `SourceBuffer.Feed` (`opus/source.go:51`)      | Producer-side: drops the oldest frame when the ring is full (cap 3 = 60ms). Pooled `PCM`/`Opus` buffers returned in place. |
| **VoiceProvider** | `ProvideOpusFrame` (`opus/voice_provider.go:61`) | Bleed-off: when `len(ch) > providerDrainThreshold` (3 = 60ms), drops exactly one extra frame per call.                     |

Each drop produces a 20ms gap — far less noticeable than cumulative delay from playing stale frames in order. The provider keeps the most recent frame so speech is not cut mid-word at drain boundaries.

---

## Empty-channel pause

Channel mixers are **paused** when their destination voice channel has no non-bot users or when no input frames have arrived for `DrainIdleTimeout` (5s). A paused mixer calls `src.Drain()` on every input (releasing pooled buffers) and skips mixing, encoding, and sink invocation — eliminating the Opus encode cost for silent or unwatched channels.

| Trigger                | Source                                  | Effect                                                                              |
|------------------------|-----------------------------------------|-------------------------------------------------------------------------------------|
| `GuildVoiceJoin`       | `bot/handlers.go:onVoiceJoin`           | `UpdateMixerPause` → unpause if channel now has a listener                          |
| `GuildVoiceLeave`      | `bot/handlers.go:onVoiceLeave`          | `UpdateMixerPause` → pause if channel is now empty                                  |
| `GuildVoiceMove`       | `bot/handlers.go:onVoiceMove`           | `UpdateMixerPause` → re-evaluate both old and new channel                           |
| Session start          | `manager/service.go:syncMixerPauseState` | Set initial pause state for all channel mixers based on current voice states        |
| No input frames for 5s | `opus/drain.go:DrainWatcher.Run`        | Auto-pause via `Mixer.IdleFor() > DrainIdleTimeout` poll loop; resumes on next Feed |

Pause state is stored per-session in `Session.ChannelMixers` (`channelID → MixerPauser`). The `VoiceProvider` for a paused channel receives no frames, so Discord gets silence naturally.
