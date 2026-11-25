package manager

import (
	"slices"
	"sync"
	"sync/atomic"

	"github.com/disgoorg/snowflake/v2"
)

// guildView is one guild's voice occupancy as a raid sees it: the non-bot
// members the owner bot's caches place in each channel, with their roles.
// Published whole and never changed afterwards.
type guildView struct {
	channels map[snowflake.ID][]viewMember
	roles    map[snowflake.ID][]snowflake.ID // member in voice → their roles
}

type viewMember struct {
	id    snowflake.ID
	roles []snowflake.ID
}

// viewProbe is the router's VoiceProbe and the capture filter's source. It
// answers from the last guildView the service published, so neither reads
// the caches itself.
type viewProbe struct {
	refreshMu sync.Mutex // orders build+publish, so an older build never replaces a newer one
	view      atomic.Pointer[guildView]
	// watcher, when set, is told of every published view, in publish order.
	watcher atomic.Pointer[func(*viewProbe)]
}

func (p *viewProbe) HasListeners(channelID snowflake.ID) bool {
	return len(p.view.Load().channels[channelID]) > 0
}

// EnumerateCallers returns the members in channelID who hold roleID. Nobody
// holds role 0, which stands for "no caller role bound".
func (p *viewProbe) EnumerateCallers(channelID, roleID snowflake.ID) []snowflake.ID {
	var users []snowflake.ID
	for _, member := range p.view.Load().channels[channelID] {
		if slices.Contains(member.roles, roleID) {
			users = append(users, member.id)
		}
	}
	return users
}

// holdsRole reports whether userID is in voice holding roleID.
func (p *viewProbe) holdsRole(userID, roleID snowflake.ID) bool {
	return slices.Contains(p.view.Load().roles[userID], roleID)
}

// openGuildView publishes a first view of guildID for a raid and keeps it
// current through RefreshVoiceView until closeGuildView.
func (m *Service) openGuildView(guildID snowflake.ID) *viewProbe {
	p := &viewProbe{}
	p.view.Store(m.buildGuildView(guildID))
	m.views.Store(guildID, p)
	return p
}

func (m *Service) closeGuildView(guildID snowflake.ID) {
	m.views.Delete(guildID)
}

// RefreshVoiceView republishes guildID's view from the owner bot's caches.
// Event handlers call it after every voice-state or member change; it is a
// no-op for a guild without a raid.
func (m *Service) RefreshVoiceView(guildID snowflake.ID) {
	v, ok := m.views.Load(guildID)
	if !ok {
		return
	}
	p := v.(*viewProbe)
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	p.view.Store(m.buildGuildView(guildID))
	if w := p.watcher.Load(); w != nil {
		(*w)(p)
	}
}

func (m *Service) buildGuildView(guildID snowflake.ID) *guildView {
	caches := m.ownerClient.Caches
	view := &guildView{channels: map[snowflake.ID][]viewMember{}, roles: map[snowflake.ID][]snowflake.ID{}}
	for vs := range caches.VoiceStates(guildID) {
		if vs.ChannelID == nil {
			continue
		}
		member, ok := caches.Member(guildID, vs.UserID)
		if !ok || m.IsBot(member.User) {
			continue
		}
		roles := slices.Clone(member.RoleIDs)
		view.roles[vs.UserID] = roles
		view.channels[*vs.ChannelID] = append(view.channels[*vs.ChannelID], viewMember{id: vs.UserID, roles: roles})
	}
	return view
}
