package manager

import (
	"path/filepath"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/config"
	"github.com/sealbro/go-discord-caller/internal/store"
)

// A speaker disabled in /setup must stay disabled across a restart. Seeding
// re-registers every pool bot in the guild as enabled, so a toggle held only in
// memory is undone by the next process start, and the raid then waits out the
// full join budget on a bot the operator switched off. Observed in production:
// GoSpeaker09 was disabled, the process restarted 2026-10-05T16:20Z, and the
// owner bot joined 20 s late in every raid after that.
func TestToggleSpeaker_DisabledSurvivesRestart(t *testing.T) {
	const speakerID = snowflake.ID(500)
	path := filepath.Join(t.TempDir(), "store.yaml")

	startProcess := func() (*Service, *store.YAMLStore) {
		st, err := store.NewYAMLStore(path)
		if err != nil {
			t.Fatalf("NewYAMLStore: %v", err)
		}
		speakerCaches := cache.New()
		speakerCaches.SetSelfUser(discord.OAuth2User{User: discord.User{ID: speakerID, Username: "speaker"}})
		ownerCaches := cache.New(cache.WithCaches(cache.FlagsAll))
		ownerCaches.AddMember(discord.Member{GuildID: testGuildID, User: discord.User{ID: speakerID}})

		m := NewService(st,
			onePool{id: speakerID, client: &bot.Client{Caches: speakerCaches}},
			&bot.Client{Caches: ownerCaches},
			testBotID, config.TestConfig{}, nil)
		m.seedGuildSpeakers(testGuildID, testBotID)
		return m, st
	}

	m, st := startProcess()
	if err := m.ToggleSpeaker(testGuildID, speakerID, false); err != nil {
		t.Fatalf("ToggleSpeaker: %v", err)
	}
	st.Close()

	restarted, st := startProcess()
	defer st.Close()

	speakers, err := restarted.snapshotSpeakers(testGuildID)
	if err != nil {
		t.Fatalf("snapshotSpeakers: %v", err)
	}
	if len(speakers) != 1 {
		t.Fatalf("want the speaker re-seeded after restart, got %d speakers", len(speakers))
	}
	if speakers[0].Enabled {
		t.Error("speaker disabled before the restart came back enabled after it")
	}
}
