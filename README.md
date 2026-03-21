# go-discord-caller
Go discord caller bot

Allow you to create a discord owner bot with many speakers bots (gateways) that can hear a discord voice channel and catch users voice with a specific role and replay this voice to all bot speakers.
Every speaker can bind to a specific voice channel and catch users' voice and replay this voice to all bot speakers.
Every speaker has access to a list of voice channels which allow him.

## Features

- [ ] Bots
  - [ ] Speaker bot
    - [ ] Catch users voice with a specific role
    - [ ] Replay voice to all bot speakers
    - [ ] Join/Leave a voice channel and listen to it
    - [ ] Don't replay voice to the voice channel where the speaker bot is
  - [ ] Manager bot
    - [ ] Discord bot commands
        - [ ] Role bot admin
          - [ ] '/setup-speakers' List all speakers
            - [ ] Show speaker name (added speaker bot username), show combobox with allowed voice channels to bind
            - [ ] Add speaker bot to a discord server, at the end button to add new speaker to the server, allow add if has not added speaker bot token
            - [ ] Checkbox do enable/disable a speaker bot without removing it from the server
          - [ ] Bind/Unbind a voice channel to a speaker (voice channel id to speaker id)
          - [ ] Bind/Unbind a role which will be able to catch users voice
        - [ ] Role bot manager
          - [ ] '/start-voice-raid', '/stop-voice-raid' Allow start/stop voice session when all speakers bot joined to discord voice channels
        - [ ] Others
          - [ ] '/status' List all voice channels bound to a speaker
          - [ ] '/status' List all users with a specific role