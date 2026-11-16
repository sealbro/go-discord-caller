package bot

import (
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

const (
	cacheTestGuild   snowflake.ID = 100
	cacheTestChannel snowflake.ID = 200
)

func newOwnerCaches() cache.Caches {
	var caches cache.Caches
	caches = cache.New(ownerCacheOpts(&caches)...)
	return caches
}

func testMember(id snowflake.ID, isBot bool) discord.Member {
	return discord.Member{
		GuildID: cacheTestGuild,
		User:    discord.User{ID: id, Bot: isBot},
		RoleIDs: []snowflake.ID{1},
	}
}

func joinVoice(caches cache.Caches, userID snowflake.ID) {
	ch := cacheTestChannel
	caches.AddVoiceState(discord.VoiceState{GuildID: cacheTestGuild, UserID: userID, ChannelID: &ch})
}

func isCached(caches cache.Caches, userID snowflake.ID) bool {
	_, ok := caches.Member(cacheTestGuild, userID)
	return ok
}

func TestOwnerCacheKeepsOnlyVoiceMembersAndBots(t *testing.T) {
	caches := newOwnerCaches()

	caches.AddMember(testMember(1, false))
	if isCached(caches, 1) {
		t.Error("member outside voice was cached")
	}

	caches.AddMember(testMember(2, true))
	if !isCached(caches, 2) {
		t.Error("bot outside voice was not cached")
	}

	joinVoice(caches, 3)
	caches.AddMember(testMember(3, false))
	if !isCached(caches, 3) {
		t.Error("member in voice was not cached")
	}
}

// disgo's GUILD_CREATE handler adds members before voice states.
func TestCacheVoiceMembersRestoresMembersAlreadyInVoiceAtGuildCreate(t *testing.T) {
	caches := newOwnerCaches()
	inVoice, idle := testMember(1, false), testMember(2, false)

	caches.AddMember(inVoice)
	caches.AddMember(idle)
	joinVoice(caches, inVoice.User.ID)

	cacheVoiceMembers(caches, cacheTestGuild, []discord.Member{inVoice, idle})

	if !isCached(caches, inVoice.User.ID) {
		t.Error("member in voice at GUILD_CREATE is missing from the cache")
	}
	if isCached(caches, idle.User.ID) {
		t.Error("member outside voice at GUILD_CREATE was cached")
	}
}

func TestEvictVoiceLeaverDropsHumansButKeepsBots(t *testing.T) {
	caches := newOwnerCaches()
	human, bot := testMember(1, false), testMember(2, true)
	for _, m := range []discord.Member{human, bot} {
		joinVoice(caches, m.User.ID)
		caches.AddMember(m)
	}

	for _, m := range []discord.Member{human, bot} {
		caches.RemoveVoiceState(cacheTestGuild, m.User.ID)
		evictVoiceLeaver(caches, m)
	}

	if isCached(caches, human.User.ID) {
		t.Error("member stayed cached after leaving voice")
	}
	if !isCached(caches, bot.User.ID) {
		t.Error("bot was evicted after leaving voice")
	}
}

func fireMemberUpdate(caches cache.Caches, m discord.Member) *fakeManager {
	f := &fakeManager{}
	client := &bot.Client{Caches: caches}
	onGuildMemberUpdate(f)(&events.GuildMemberUpdate{
		GenericGuildMember: &events.GenericGuildMember{
			GenericEvent: events.NewGenericEvent(client, 0, 0),
			GuildID:      cacheTestGuild,
			Member:       m,
		},
	})
	return f
}

func TestMemberUpdateDoesNotCacheMembersOutsideVoice(t *testing.T) {
	caches := newOwnerCaches()

	f := fireMemberUpdate(caches, testMember(1, false))

	if isCached(caches, 1) {
		t.Error("member update cached a user who is not in voice")
	}
	if got := len(f.snapshotCalls().notifyMemberCalls); got != 1 {
		t.Errorf("NotifyMemberUpdate calls = %d, want 1", got)
	}
}

func TestMemberUpdateRefreshesMembersInVoice(t *testing.T) {
	caches := newOwnerCaches()
	joinVoice(caches, 2)
	caches.AddMember(testMember(2, false))

	updated := testMember(2, false)
	updated.RoleIDs = []snowflake.ID{7}
	fireMemberUpdate(caches, updated)

	got, ok := caches.Member(cacheTestGuild, 2)
	if !ok || len(got.RoleIDs) != 1 || got.RoleIDs[0] != 7 {
		t.Errorf("cached member = %+v (ok %v), want the updated roles", got.RoleIDs, ok)
	}
}

func TestOwnerCacheStoresNoMessages(t *testing.T) {
	caches := newOwnerCaches()

	caches.AddMessage(discord.Message{ID: 1, ChannelID: cacheTestChannel})

	if _, ok := caches.Message(cacheTestChannel, 1); ok {
		t.Error("owner cache stored a message")
	}
}
