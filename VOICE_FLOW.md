# Voice Session Flow

---

## RaidModeOneCaller — owner capture only

Only the owner bot has a `VoiceReceiver`. Speakers are `VoiceProvider`-only.
Single source → no mix-minus needed → each `ChannelMixer` has exactly one input.
`chOwnerOut` is **not** created (owner does not play back audio into its own channel).

```mermaid
flowchart TD
    subgraph HOST_GUILD["Host Guild — RaidModeOneCaller"]
        subgraph ChA["Discord Channel A (owner)"]
            OwnerVR["Owner VoiceReceiver"]
        end
        subgraph ChB["Discord Channel B"]
            Spk1VP["Speaker1 VoiceProvider"]
        end
        subgraph ChC["Discord Channel C"]
            Spk2VP["Speaker2 VoiceProvider"]
        end

        chIn["chIn (owner capture)"]
        OwnerVR --> chIn

        FanA["Fanout A (goroutine)"]
        chIn --> FanA

        MixB["ChannelMixer B (1 input)"]
        MixC["ChannelMixer C (1 input)"]
        RelayMix["RelayMixer (1 input)"]

        FanA -- "mixCh_B"   --> MixB
        FanA -- "mixCh_C"   --> MixC
        FanA -- "relayCh_A" --> RelayMix

        chOutB["chOut (spk1)"]
        chOutC["chOut (spk2)"]

        MixB -- "Output()" --> chOutB
        MixC -- "Output()" --> chOutC

        chOutB --> Spk1VP
        chOutC --> Spk2VP
    end

    subgraph RELAY["Inter-Guild Relay"]
        AllySession["ally.Session BroadcastFromGuild()"]
        RelayMix -- "Output()" --> AllySession
    end

    subgraph GUEST_GUILD["Guest Guild"]
        GuestOwnerVP["Guest Owner VoiceProvider"]
        GuestSpkVP["Guest Speaker VoiceProvider(s)"]
        AllySession -- "chOut (owner)"    --> GuestOwnerVP
        AllySession -- "chOut (speakers)" --> GuestSpkVP
    end

    %% link indices:
    %%  0  : OwnerVR → chIn
    %%  1  : chIn → FanA
    %%  2  : FanA → MixB
    %%  3  : FanA → MixC
    %%  4  : FanA → RelayMix
    %%  5  : MixB → chOutB
    %%  6  : MixC → chOutC
    %%  7  : chOutB → Spk1VP
    %%  8  : chOutC → Spk2VP
    %%  9  : RelayMix → AllySession
    %%  10 : AllySession → GuestOwnerVP
    %%  11 : AllySession → GuestSpkVP

    %% Owner capture path — blue shades, dark→light
    linkStyle 0 stroke:#0d47a1,stroke-width:2px
    linkStyle 1 stroke:#1565c0,stroke-width:2px

    %% Fanout → ChannelMixer B — green shades
    linkStyle 2 stroke:#1b5e20,stroke-width:2px
    linkStyle 5 stroke:#66bb6a,stroke-width:2px
    linkStyle 7 stroke:#a5d6a7,stroke-width:2px

    %% Fanout → ChannelMixer C — orange shades
    linkStyle 3 stroke:#e65100,stroke-width:2px
    linkStyle 6 stroke:#ffa726,stroke-width:2px
    linkStyle 8 stroke:#ffcc80,stroke-width:2px

    %% Relay path — purple shades
    linkStyle 4  stroke:#4a148c,stroke-width:2px
    linkStyle 9  stroke:#ba68c8,stroke-width:2px

    %% Guest delivery — red shades
    linkStyle 10 stroke:#b71c1c,stroke-width:2px
    linkStyle 11 stroke:#e57373,stroke-width:2px
```

---

## RaidModeGuildCaller — all channels capture and relay

Every channel has both a `VoiceReceiver` (capture) and a `VoiceProvider` (playback).
Each `ChannelMixer` receives audio from all sources **except its own channel** (mix-minus).
The owner bot also gets `chOwnerOut` so it plays back the mixed audio of other channels.

```mermaid
flowchart TD
    subgraph HOST_GUILD["Host Guild — RaidModeGuildCaller"]
        subgraph ChA["Discord Channel A"]
            OwnerVR["Owner VoiceReceiver"]
            OwnerVP["Owner VoiceProvider"]
        end
        subgraph ChB["Discord Channel B"]
            Spk1VR["Speaker1 VoiceReceiver"]
            Spk1VP["Speaker1 VoiceProvider"]
        end
        subgraph ChC["Discord Channel C"]
            Spk2VR["Speaker2 VoiceReceiver"]
            Spk2VP["Speaker2 VoiceProvider"]
        end

        chIn["chIn (owner capture)"]
        chCaptureB["chCapture (spk1 capture)"]
        chCaptureC["chCapture (spk2 capture)"]

        OwnerVR --> chIn
        Spk1VR --> chCaptureB
        Spk2VR --> chCaptureC

        FanA["Fanout A (goroutine)"]
        FanB["Fanout B (goroutine)"]
        FanC["Fanout C (goroutine)"]

        chIn       --> FanA
        chCaptureB --> FanB
        chCaptureC --> FanC

        MixA["ChannelMixer A (mix-minus Ch A)"]
        MixB["ChannelMixer B (mix-minus Ch B)"]
        MixC["ChannelMixer C (mix-minus Ch C)"]
        RelayMix["RelayMixer (all sources)"]

        %% Fanout A feeds all channel mixers except A (mix-minus) and relay
        FanA -- "mixCh_B" --> MixB
        FanA -- "mixCh_C" --> MixC
        FanA -- "relayCh_A" --> RelayMix

        %% Fanout B skips MixB (mix-minus)
        FanB -- "mixCh_A" --> MixA
        FanB -- "mixCh_C" --> MixC
        FanB -- "relayCh_B" --> RelayMix

        %% Fanout C skips MixC (mix-minus)
        FanC -- "mixCh_A" --> MixA
        FanC -- "mixCh_B" --> MixB
        FanC -- "relayCh_C" --> RelayMix

        chOwnerOut["chOwnerOut"]
        chOutB["chOut (spk1)"]
        chOutC["chOut (spk2)"]

        MixA -- "Output()" --> chOwnerOut
        MixB -- "Output()" --> chOutB
        MixC -- "Output()" --> chOutC

        chOwnerOut --> OwnerVP
        chOutB     --> Spk1VP
        chOutC     --> Spk2VP
    end

    subgraph RELAY["Inter-Guild Relay"]
        AllySession["ally.Session BroadcastFromGuild()"]
        RelayMix -- "Output()" --> AllySession
    end

    subgraph GUEST_GUILD["Guest Guild"]
        GuestOwnerVP["Guest Owner VoiceProvider"]
        GuestSpkVP["Guest Speaker VoiceProvider(s)"]
        AllySession -- "chOut (owner)"   --> GuestOwnerVP
        AllySession -- "chOut (speakers)"--> GuestSpkVP
    end

    %% link indices (0-based, in definition order):
    %%  0-2   : VoiceReceiver → capture channel   (blue)
    %%  3-5   : capture channel → fanout           (cyan)
    %%  6,7   : FanA → ChannelMixer B/C            (green)
    %%  8     : FanA → RelayMixer                  (orange)
    %%  9,10  : FanB → ChannelMixer A/C            (green)
    %%  11    : FanB → RelayMixer                  (orange)
    %%  12,13 : FanC → ChannelMixer A/B            (green)
    %%  14    : FanC → RelayMixer                  (orange)
    %%  15-17 : ChannelMixer → chOut               (purple)
    %%  18-20 : chOut → VoiceProvider              (pink)
    %%  21    : RelayMixer → AllySession            (orange)
    %%  22-23 : AllySession → Guest VoiceProviders (red)

    %% Channel A (Owner) — blue shades, dark→light: capture in, mix inputs, mix out, provider
    linkStyle 0  stroke:#0d47a1,stroke-width:2px
    linkStyle 3  stroke:#1565c0,stroke-width:2px
    linkStyle 9  stroke:#1976d2,stroke-width:2px
    linkStyle 12 stroke:#1e88e5,stroke-width:2px
    linkStyle 15 stroke:#42a5f5,stroke-width:2px
    linkStyle 18 stroke:#90caf9,stroke-width:2px

    %% Channel B (Speaker1) — green shades, dark→light
    linkStyle 1  stroke:#1b5e20,stroke-width:2px
    linkStyle 4  stroke:#2e7d32,stroke-width:2px
    linkStyle 6  stroke:#388e3c,stroke-width:2px
    linkStyle 13 stroke:#43a047,stroke-width:2px
    linkStyle 16 stroke:#66bb6a,stroke-width:2px
    linkStyle 19 stroke:#a5d6a7,stroke-width:2px

    %% Channel C (Speaker2) — orange shades, dark→light
    linkStyle 2  stroke:#e65100,stroke-width:2px
    linkStyle 5  stroke:#ef6c00,stroke-width:2px
    linkStyle 7  stroke:#f57c00,stroke-width:2px
    linkStyle 10 stroke:#fb8c00,stroke-width:2px
    linkStyle 17 stroke:#ffa726,stroke-width:2px
    linkStyle 20 stroke:#ffcc80,stroke-width:2px

    %% Relay path — purple shades
    linkStyle 8  stroke:#4a148c,stroke-width:2px
    linkStyle 11 stroke:#6a1b9a,stroke-width:2px
    linkStyle 14 stroke:#8e24aa,stroke-width:2px
    linkStyle 21 stroke:#ba68c8,stroke-width:2px

    %% Guest delivery — red shades
    linkStyle 22 stroke:#b71c1c,stroke-width:2px
    linkStyle 23 stroke:#e57373,stroke-width:2px
```

---

## RaidModeGuildCaller + RaidModeAllyCaller — bi-directional inter-guild relay

Both guilds have a full mix-minus graph. Both use `BroadcastFromGuild` so neither
hears its own audio echoed back. `registerRelayInputs` wires relay inputs into each
guild's `ChannelMixer`s so incoming relay audio is mixed alongside local sources.

- **Host → Guest**: `RelayMixer` → `BroadcastFromGuild(hostID)` → guest `relayOpusIn` → Bridge (decode once) → guest `ChannelMixer`s
- **Guest → Host**: guest `RelayMixer` → `BroadcastFromGuild(guestID)` → host `relayOpusIn` → Bridge (decode once) → host `ChannelMixer`s

Each relay bridge is a **single goroutine** with a **single decoder** that fans the resulting
`opus.Frame` to all destination channel mixers — one decode per packet regardless of how
many destination channels exist.

```mermaid
flowchart TD
    subgraph HOST["Host Guild — RaidModeGuildCaller"]
        subgraph HChA["Channel A (owner)"]
            HOwnerVR["Owner VoiceReceiver"]
            HOwnerVP["Owner VoiceProvider"]
        end
        subgraph HChB["Channel B"]
            HSpkVR["Speaker VoiceReceiver"]
            HSpkVP["Speaker VoiceProvider"]
        end

        HchIn["chIn"]
        HchCap["chCapture"]
        HFanA["Fanout A"]
        HFanB["Fanout B"]
        HMixA["ChannelMixer A (mix-minus)"]
        HMixB["ChannelMixer B (mix-minus)"]
        HRelayMix["RelayMixer"]
        HchOwnerOut["chOwnerOut"]
        HchOut["chOut (spk)"]
        HRelayIn["relayOpusIn (host)"]
        HBridge["Bridge (decode once)"]

        HOwnerVR --> HchIn  --> HFanA
        HSpkVR   --> HchCap --> HFanB

        HFanA -- "mixCh_B"   --> HMixB
        HFanA -- "relayCh_A" --> HRelayMix
        HFanB -- "mixCh_A"   --> HMixA
        HFanB -- "relayCh_B" --> HRelayMix

        HRelayIn --> HBridge
        HBridge -- "Frame A" --> HMixA
        HBridge -- "Frame B" --> HMixB

        HMixA -- "Output()" --> HchOwnerOut --> HOwnerVP
        HMixB -- "Output()" --> HchOut      --> HSpkVP
    end

    subgraph SESSION["ally.Session"]
        BFGHost["BroadcastFromGuild(hostID)"]
        BFGGuest["BroadcastFromGuild(guestID)"]
    end

    HRelayMix -- "Output()" --> BFGHost

    subgraph GUEST["Guest Guild — RaidModeAllyCaller"]
        subgraph GChA["Channel A (owner)"]
            GOwnerVR["Owner VoiceReceiver"]
            GOwnerVP["Owner VoiceProvider"]
        end
        subgraph GChB["Channel B"]
            GSpkVR["Speaker VoiceReceiver"]
            GSpkVP["Speaker VoiceProvider"]
        end

        GchIn["chIn (owner capture)"]
        GchCap["chCapture"]
        GFanA["Fanout A"]
        GFanB["Fanout B"]
        GMixA["ChannelMixer A (mix-minus)"]
        GMixB["ChannelMixer B (mix-minus)"]
        GRelayMix["RelayMixer"]
        GchOwnerOut["chOwnerOut"]
        GchOut["chOut (spk)"]
        GRelayIn["relayOpusIn (guest)"]
        GBridge["Bridge (decode once)"]

        GOwnerVR --> GchIn  --> GFanA
        GSpkVR   --> GchCap --> GFanB

        GFanA -- "mixCh_B"   --> GMixB
        GFanA -- "relayCh_A" --> GRelayMix

        GFanB -- "mixCh_A"   --> GMixA
        GFanB -- "relayCh_B" --> GRelayMix

        GRelayIn --> GBridge
        GBridge -- "Frame A" --> GMixA
        GBridge -- "Frame B" --> GMixB

        GMixA -- "Output()" --> GchOwnerOut --> GOwnerVP
        GMixB -- "Output()" --> GchOut      --> GSpkVP
    end

    GRelayMix -- "Output()" --> BFGGuest

    BFGHost  --> GRelayIn
    BFGGuest --> HRelayIn

    %% link indices:
    %%  0  : HOwnerVR → HchIn
    %%  1  : HchIn → HFanA
    %%  2  : HSpkVR → HchCap
    %%  3  : HchCap → HFanB
    %%  4  : HFanA → HMixB
    %%  5  : HFanA → HRelayMix
    %%  6  : HFanB → HMixA
    %%  7  : HFanB → HRelayMix
    %%  8  : HRelayIn → HBridge
    %%  9  : HBridge → HMixA (Frame A)
    %%  10 : HBridge → HMixB (Frame B)
    %%  11 : HMixA → HchOwnerOut
    %%  12 : HchOwnerOut → HOwnerVP
    %%  13 : HMixB → HchOut
    %%  14 : HchOut → HSpkVP
    %%  15 : HRelayMix → BFGHost
    %%  16 : GOwnerVR → GchIn
    %%  17 : GchIn → GFanA
    %%  18 : GSpkVR → GchCap
    %%  19 : GchCap → GFanB
    %%  20 : GFanA → GMixB
    %%  21 : GFanA → GRelayMix
    %%  22 : GFanB → GMixA
    %%  23 : GFanB → GRelayMix
    %%  24 : GRelayIn → GBridge
    %%  25 : GBridge → GMixA (Frame A)
    %%  26 : GBridge → GMixB (Frame B)
    %%  27 : GMixA → GchOwnerOut
    %%  28 : GchOwnerOut → GOwnerVP
    %%  29 : GMixB → GchOut
    %%  30 : GchOut → GSpkVP
    %%  31 : GRelayMix → BFGGuest
    %%  32 : BFGHost → GRelayIn
    %%  33 : BFGGuest → HRelayIn

    %% Host Ch A (owner) — blue shades, dark→light
    linkStyle 0  stroke:#0d47a1,stroke-width:2px
    linkStyle 1  stroke:#1565c0,stroke-width:2px
    linkStyle 6  stroke:#1976d2,stroke-width:2px
    linkStyle 8  stroke:#1e88e5,stroke-width:2px
    linkStyle 9  stroke:#2196f3,stroke-width:2px
    linkStyle 11 stroke:#42a5f5,stroke-width:2px
    linkStyle 12 stroke:#90caf9,stroke-width:2px

    %% Host Ch B (speaker) — green shades, dark→light
    linkStyle 2  stroke:#1b5e20,stroke-width:2px
    linkStyle 3  stroke:#2e7d32,stroke-width:2px
    linkStyle 4  stroke:#388e3c,stroke-width:2px
    linkStyle 10 stroke:#43a047,stroke-width:2px
    linkStyle 13 stroke:#66bb6a,stroke-width:2px
    linkStyle 14 stroke:#a5d6a7,stroke-width:2px

    %% Host relay — purple shades
    linkStyle 5  stroke:#4a148c,stroke-width:2px
    linkStyle 7  stroke:#6a1b9a,stroke-width:2px
    linkStyle 15 stroke:#ba68c8,stroke-width:2px

    %% Guest Ch A (owner) capture — cyan shades, dark→light
    linkStyle 16 stroke:#006064,stroke-width:2px
    linkStyle 17 stroke:#00838f,stroke-width:2px
    linkStyle 22 stroke:#0097a7,stroke-width:2px
    linkStyle 24 stroke:#00acc1,stroke-width:2px

    %% Guest Ch B (speaker) capture — orange shades, dark→light
    linkStyle 18 stroke:#e65100,stroke-width:2px
    linkStyle 19 stroke:#ef6c00,stroke-width:2px
    linkStyle 20 stroke:#f57c00,stroke-width:2px
    linkStyle 23 stroke:#ffa726,stroke-width:2px

    %% Guest Ch A (owner) output — red shades
    linkStyle 25 stroke:#b71c1c,stroke-width:2px
    linkStyle 27 stroke:#e53935,stroke-width:2px
    linkStyle 28 stroke:#e57373,stroke-width:2px

    %% Guest Ch B output — pink shades
    linkStyle 26 stroke:#880e4f,stroke-width:2px
    linkStyle 29 stroke:#c2185b,stroke-width:2px
    linkStyle 30 stroke:#f06292,stroke-width:2px

    %% Guest relay — purple shades (lighter than host)
    linkStyle 21 stroke:#ce93d8,stroke-width:2px
    linkStyle 31 stroke:#ce93d8,stroke-width:2px

    %% Host → Guest relay delivery — teal
    linkStyle 32 stroke:#00695c,stroke-width:2px

    %% Guest → Host relay delivery — gold
    linkStyle 33 stroke:#f57f17,stroke-width:2px
```

---

## RaidModeOneManyGuildCaller — star topology (host)

Star topology: the owner is the central hub. Speakers capture audio but each
speaker's fanout targets **only the owner's ChannelMixer** — speakers cannot
hear each other. The owner's fanout uses standard mix-minus (all channels
except its own). Guest relay inputs also target only the owner's ChannelMixer.

```mermaid
flowchart TD
    subgraph HOST_GUILD["Host Guild — RaidModeOneManyGuildCaller"]
        subgraph ChA["Discord Channel A (owner = hub)"]
            OwnerVR["Owner VoiceReceiver"]
            OwnerVP["Owner VoiceProvider"]
        end
        subgraph ChB["Discord Channel B"]
            Spk1VR["Speaker1 VoiceReceiver"]
            Spk1VP["Speaker1 VoiceProvider"]
        end
        subgraph ChC["Discord Channel C"]
            Spk2VR["Speaker2 VoiceReceiver"]
            Spk2VP["Speaker2 VoiceProvider"]
        end

        chIn["chIn (owner capture)"]
        chCaptureB["chCapture (spk1 capture)"]
        chCaptureC["chCapture (spk2 capture)"]

        OwnerVR --> chIn
        Spk1VR --> chCaptureB
        Spk2VR --> chCaptureC

        FanA["Fanout A (goroutine)"]
        FanB["Fanout B (goroutine)"]
        FanC["Fanout C (goroutine)"]

        chIn       --> FanA
        chCaptureB --> FanB
        chCaptureC --> FanC

        MixA["ChannelMixer A (hub — all spk sources)"]
        MixB["ChannelMixer B (owner only)"]
        MixC["ChannelMixer C (owner only)"]
        RelayMix["RelayMixer (all sources)"]

        %% Owner fanout → all channel mixers except A (mix-minus) + relay
        FanA -- "mixCh_B" --> MixB
        FanA -- "mixCh_C" --> MixC
        FanA -- "relayCh_A" --> RelayMix

        %% Speaker B fanout → owner mixer A ONLY (star spoke → hub) + relay
        FanB -- "mixCh_A" --> MixA
        FanB -- "relayCh_B" --> RelayMix

        %% Speaker C fanout → owner mixer A ONLY (star spoke → hub) + relay
        FanC -- "mixCh_A" --> MixA
        FanC -- "relayCh_C" --> RelayMix

        HRelayInA["relayIn A (guest → owner only)"]
        HRelayInA --> MixA

        chOwnerOut["chOwnerOut"]
        chOutB["chOut (spk1)"]
        chOutC["chOut (spk2)"]

        MixA -- "Output()" --> chOwnerOut
        MixB -- "Output()" --> chOutB
        MixC -- "Output()" --> chOutC

        chOwnerOut --> OwnerVP
        chOutB     --> Spk1VP
        chOutC     --> Spk2VP
    end

    subgraph RELAY["Inter-Guild Relay"]
        AllySession["ally.Session BroadcastFromGuild()"]
        RelayMix -- "Output()" --> AllySession
    end

    subgraph GUEST_GUILD["Guest Guild"]
        GuestOwnerVP["Guest Owner VoiceProvider"]
        GuestSpkVP["Guest Speaker VoiceProvider(s)"]
        AllySession -- "chOut (owner)"    --> GuestOwnerVP
        AllySession -- "chOut (speakers)" --> GuestSpkVP
    end

    %% link indices:
    %%  0-2   : VoiceReceiver → capture channel
    %%  3-5   : capture channel → fanout
    %%  6,7   : FanA → MixB, MixC (owner → speakers, mix-minus)
    %%  8     : FanA → RelayMix
    %%  9     : FanB → MixA (spk1 → owner ONLY)
    %%  10    : FanB → RelayMix
    %%  11    : FanC → MixA (spk2 → owner ONLY)
    %%  12    : FanC → RelayMix
    %%  13    : HRelayInA → MixA
    %%  14-16 : MixA/B/C → chOut
    %%  17-19 : chOut → VoiceProvider
    %%  20    : RelayMix → AllySession
    %%  21-22 : AllySession → Guest VoiceProviders

    %% Channel A (Owner hub) — blue shades
    linkStyle 0  stroke:#0d47a1,stroke-width:2px
    linkStyle 3  stroke:#1565c0,stroke-width:2px
    linkStyle 9  stroke:#1976d2,stroke-width:2px
    linkStyle 11 stroke:#1e88e5,stroke-width:2px
    linkStyle 13 stroke:#2196f3,stroke-width:2px
    linkStyle 14 stroke:#42a5f5,stroke-width:2px
    linkStyle 17 stroke:#90caf9,stroke-width:2px

    %% Channel B (Speaker1) — green shades
    linkStyle 1  stroke:#1b5e20,stroke-width:2px
    linkStyle 4  stroke:#2e7d32,stroke-width:2px
    linkStyle 6  stroke:#388e3c,stroke-width:2px
    linkStyle 15 stroke:#66bb6a,stroke-width:2px
    linkStyle 18 stroke:#a5d6a7,stroke-width:2px

    %% Channel C (Speaker2) — orange shades
    linkStyle 2  stroke:#e65100,stroke-width:2px
    linkStyle 5  stroke:#ef6c00,stroke-width:2px
    linkStyle 7  stroke:#f57c00,stroke-width:2px
    linkStyle 16 stroke:#ffa726,stroke-width:2px
    linkStyle 19 stroke:#ffcc80,stroke-width:2px

    %% Relay path — purple shades
    linkStyle 8  stroke:#4a148c,stroke-width:2px
    linkStyle 10 stroke:#6a1b9a,stroke-width:2px
    linkStyle 12 stroke:#8e24aa,stroke-width:2px
    linkStyle 20 stroke:#ba68c8,stroke-width:2px

    %% Guest delivery — red shades
    linkStyle 21 stroke:#b71c1c,stroke-width:2px
    linkStyle 22 stroke:#e57373,stroke-width:2px
```

---

## RaidModeOneManyGuildCaller + RaidModeOneManyAllyCaller — star topology inter-guild

Both guilds use the star topology. The host owner is the central hub across guilds.

- **Host**: Owner hears all local speakers + guest relay. Speakers hear only the owner.
  Guest relay input is wired to the owner's ChannelMixer only.
- **Guest**: All captures go to relay only (no local cross-channel mixing). Channel mixers
  receive audio solely from `registerRelayInputs` (host's relay), so guest speakers
  hear only the host owner — not each other.

```mermaid
flowchart TD
    subgraph HOST["Host Guild — RaidModeOneManyGuildCaller"]
        subgraph HChA["Channel A (owner = hub)"]
            HOwnerVR["Owner VoiceReceiver"]
            HOwnerVP["Owner VoiceProvider"]
        end
        subgraph HChB["Channel B"]
            HSpkVR["Speaker VoiceReceiver"]
            HSpkVP["Speaker VoiceProvider"]
        end

        HchIn["chIn"]
        HchCap["chCapture"]
        HFanA["Fanout A"]
        HFanB["Fanout B"]
        HMixA["ChannelMixer A (hub)"]
        HMixB["ChannelMixer B (owner only)"]
        HRelayMix["RelayMixer"]
        HchOwnerOut["chOwnerOut"]
        HchOut["chOut (spk)"]
        HRelayIn["relayOpusIn (host)"]
        HBridge["Bridge (decode once)"]

        HOwnerVR --> HchIn  --> HFanA
        HSpkVR   --> HchCap --> HFanB

        %% Owner → speaker channel (mix-minus)
        HFanA -- "mixCh_B"   --> HMixB
        HFanA -- "relayCh_A" --> HRelayMix

        %% Speaker → owner channel ONLY (star spoke → hub)
        HFanB -- "mixCh_A"   --> HMixA
        HFanB -- "relayCh_B" --> HRelayMix

        %% Guest relay → owner mixer only (star: relay enters only at hub)
        HRelayIn --> HBridge
        HBridge -- "Frame" --> HMixA

        HMixA -- "Output()" --> HchOwnerOut --> HOwnerVP
        HMixB -- "Output()" --> HchOut      --> HSpkVP
    end

    subgraph SESSION["ally.Session"]
        BFGHost["BroadcastFromGuild(hostID)"]
        BFGGuest["BroadcastFromGuild(guestID)"]
    end

    HRelayMix -- "Output()" --> BFGHost

    subgraph GUEST["Guest Guild — RaidModeOneManyAllyCaller"]
        subgraph GChA["Channel A (owner)"]
            GOwnerVR["Owner VoiceReceiver"]
            GOwnerVP["Owner VoiceProvider"]
        end
        subgraph GChB["Channel B"]
            GSpkVR["Speaker VoiceReceiver"]
            GSpkVP["Speaker VoiceProvider"]
        end

        GchIn["chIn (owner capture)"]
        GchCap["chCapture"]
        GFanA["Fanout A"]
        GFanB["Fanout B"]
        GMixA["ChannelMixer A (relay only)"]
        GMixB["ChannelMixer B (relay only)"]
        GRelayMix["RelayMixer"]
        GchOwnerOut["chOwnerOut"]
        GchOut["chOut (spk)"]
        GRelayIn["relayOpusIn (guest)"]
        GBridge["Bridge (decode once)"]

        GOwnerVR --> GchIn  --> GFanA
        GSpkVR   --> GchCap --> GFanB

        %% Guest star: ALL sources → relay ONLY (no local channel-to-channel)
        GFanA -- "relayCh_A" --> GRelayMix
        GFanB -- "relayCh_B" --> GRelayMix

        %% Host relay → all guest channel mixers via single bridge
        GRelayIn --> GBridge
        GBridge -- "Frame A" --> GMixA
        GBridge -- "Frame B" --> GMixB

        GMixA -- "Output()" --> GchOwnerOut --> GOwnerVP
        GMixB -- "Output()" --> GchOut      --> GSpkVP
    end

    GRelayMix -- "Output()" --> BFGGuest

    BFGHost  --> GRelayIn
    BFGGuest --> HRelayIn

    %% link indices:
    %%  0  : HOwnerVR → HchIn
    %%  1  : HchIn → HFanA
    %%  2  : HSpkVR → HchCap
    %%  3  : HchCap → HFanB
    %%  4  : HFanA → HMixB (owner → spk, mix-minus)
    %%  5  : HFanA → HRelayMix
    %%  6  : HFanB → HMixA (spk → owner ONLY)
    %%  7  : HFanB → HRelayMix
    %%  8  : HRelayIn → HBridge
    %%  9  : HBridge → HMixA (Frame, owner only)
    %%  10 : HMixA → HchOwnerOut
    %%  11 : HchOwnerOut → HOwnerVP
    %%  12 : HMixB → HchOut
    %%  13 : HchOut → HSpkVP
    %%  14 : HRelayMix → BFGHost
    %%  15 : GOwnerVR → GchIn
    %%  16 : GchIn → GFanA
    %%  17 : GSpkVR → GchCap
    %%  18 : GchCap → GFanB
    %%  19 : GFanA → GRelayMix (relay only!)
    %%  20 : GFanB → GRelayMix (relay only!)
    %%  21 : GRelayIn → GBridge
    %%  22 : GBridge → GMixA (Frame A)
    %%  23 : GBridge → GMixB (Frame B)
    %%  24 : GMixA → GchOwnerOut
    %%  25 : GchOwnerOut → GOwnerVP
    %%  26 : GMixB → GchOut
    %%  27 : GchOut → GSpkVP
    %%  28 : GRelayMix → BFGGuest
    %%  29 : BFGHost → GRelayIn
    %%  30 : BFGGuest → HRelayIn

    %% Host Ch A (owner hub) — blue shades
    linkStyle 0  stroke:#0d47a1,stroke-width:2px
    linkStyle 1  stroke:#1565c0,stroke-width:2px
    linkStyle 6  stroke:#1976d2,stroke-width:2px
    linkStyle 8  stroke:#1e88e5,stroke-width:2px
    linkStyle 9  stroke:#2196f3,stroke-width:2px
    linkStyle 10 stroke:#42a5f5,stroke-width:2px
    linkStyle 11 stroke:#90caf9,stroke-width:2px

    %% Host Ch B (speaker) — green shades
    linkStyle 2  stroke:#1b5e20,stroke-width:2px
    linkStyle 3  stroke:#2e7d32,stroke-width:2px
    linkStyle 4  stroke:#388e3c,stroke-width:2px
    linkStyle 12 stroke:#66bb6a,stroke-width:2px
    linkStyle 13 stroke:#a5d6a7,stroke-width:2px

    %% Host relay — purple shades
    linkStyle 5  stroke:#4a148c,stroke-width:2px
    linkStyle 7  stroke:#6a1b9a,stroke-width:2px
    linkStyle 14 stroke:#ba68c8,stroke-width:2px

    %% Guest capture — cyan/orange shades
    linkStyle 15 stroke:#006064,stroke-width:2px
    linkStyle 16 stroke:#00838f,stroke-width:2px
    linkStyle 17 stroke:#e65100,stroke-width:2px
    linkStyle 18 stroke:#ef6c00,stroke-width:2px

    %% Guest sources → relay ONLY — teal
    linkStyle 19 stroke:#00695c,stroke-width:2px
    linkStyle 20 stroke:#26a69a,stroke-width:2px

    %% Guest relay bridge — cyan
    linkStyle 21 stroke:#00acc1,stroke-width:2px

    %% Guest channel output (from relay) — red/pink shades
    linkStyle 22 stroke:#b71c1c,stroke-width:2px
    linkStyle 23 stroke:#880e4f,stroke-width:2px
    linkStyle 24 stroke:#e53935,stroke-width:2px
    linkStyle 25 stroke:#e57373,stroke-width:2px
    linkStyle 26 stroke:#c2185b,stroke-width:2px
    linkStyle 27 stroke:#f06292,stroke-width:2px

    %% Guest relay out — purple
    linkStyle 28 stroke:#ce93d8,stroke-width:2px

    %% Host → Guest relay delivery — teal
    linkStyle 29 stroke:#00695c,stroke-width:2px

    %% Guest → Host relay delivery — gold (owner mixer only)
    linkStyle 30 stroke:#f57f17,stroke-width:2px
```

---

## Mix-minus rule

Each `ChannelMixer[X]` receives audio from **every source except sources originating in channel X**.
This prevents echo: users in channel X would otherwise hear their own audio played back to them.

### Star topology exception (OneManyGuildCaller / OneManyAllyCaller)

In star mode the mix-minus rule is tightened: speaker channel mixers receive audio **only from the
owner channel** (not from other speakers). The owner's channel mixer receives all speaker sources
(standard mix-minus still applies — it skips its own channel). Guest star mode goes further: sources
feed the relay mixer only, and channel mixers receive audio exclusively from `registerRelayInputs`.

## Data flow summary

| Stage            | Component                              | Description                                                                                           |
|------------------|----------------------------------------|-------------------------------------------------------------------------------------------------------|
| Capture          | `VoiceReceiver` → `chIn` / `chCapture`| Role-filtered Opus frames from Discord                                                                |
| Fanout           | goroutine per source                   | Decodes Opus once, distributes `Frame{PCM, Opus}` to all mixer input channels                        |
| Per-channel mix  | `ChannelMixer[X]`                      | Mixes all foreign sources; single-source passthrough forwards `Frame.Opus` directly (no copy/re-encode)|
| Relay mix        | `RelayMixer`                           | Mixes all sources; output is broadcast to every attached guest guild                                  |
| Relay bridge     | one goroutine per guild                | Decodes relay Opus **once**, fans `Frame{PCM, Opus}` to all dest `ChannelMixer` inputs               |
| Guest delivery   | `ally.Session.BroadcastFromGuild`      | Sends one Opus channel per guest guild; bridge decodes and distributes to N mixers locally            |

## Latency control — drain-to-latest

Every buffered channel in the pipeline (capture → fanout → mixer → speaker) holds up to
`audioChanBuf` (10) frames × 20 ms = **200 ms** max. Under transient stalls (GC, CPU spike)
buffers fill and latency compounds across stages. Three drain points keep end-to-end latency
near real-time:

| Drain point       | Location                        | Threshold                        | Mechanism                                                               |
|-------------------|---------------------------------|----------------------------------|-------------------------------------------------------------------------|
| **Mixer inputs**  | `Mixer.tick()` input read loop  | `mixerInputDrainThreshold` = 4   | Drops backlog frames, keeps only the freshest                           |
| **Relay bridge**  | `registerRelayInputs` goroutine | `relayBridgeDrainThreshold` = 3  | Drains `relayOpusIn` to latest before decoding; inactive under normal jitter |
| **VoiceProvider** | `ProvideOpusFrame()`            | `providerDrainThreshold` = 3     | Drops excess frames but keeps the last one, preserving speech continuity|

Dropped frames produce a brief audio gap (one 20 ms tick) — far less noticeable than
cumulative delay from playing stale frames in order. The `VoiceProvider` intentionally
keeps the final queued frame so speech is not cut mid-word at drain boundaries.

## Empty-channel pause

Channel mixers are **paused** when their destination voice channel has no non-bot users.
A paused mixer still drains its inputs (preventing upstream backpressure from fanout
goroutines) but skips mixing, encoding, and output — eliminating the Opus encode cost
for channels nobody is listening in.

| Event                       | Action                                                                   |
|-----------------------------|--------------------------------------------------------------------------|
| `GuildVoiceJoin`            | `UpdateMixerPause` → resumes mixer if channel now has a listener         |
| `GuildVoiceLeave`           | `UpdateMixerPause` → pauses mixer if channel is now empty                |
| `GuildVoiceMove`            | `UpdateMixerPause` → re-evaluates both old and new channel               |
| Session start               | `syncMixerPauseState` → sets initial pause state for all channel mixers  |

Pause state is stored per-session in `Session.ChannelMixers` (`channelID → MixerPauser`).
The `VoiceProvider` for a paused channel receives no frames, so Discord gets silence naturally.
