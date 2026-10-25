package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/ally"
	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/i18n"
	"github.com/sealbro/go-discord-caller/internal/manager"
)

const (
	testGuildID   snowflake.ID = 10
	permAdmin                  = "8"
	permManage                 = "32"
	permNone                   = "0"
	speakerA      snowflake.ID = 501
	boundChannel  snowflake.ID = 900
	selectedRole  snowflake.ID = 300
	componentBtn               = 2
	componentText              = 3
	componentRole              = 6
	componentChan              = 8
)

var en = i18n.MustBundle().For("", discord.LocaleEnglishUS)

// interactionHarness feeds raw interaction payloads through the real router,
// so routes, middleware and handlers run exactly as they do for Discord.
// Initial responses are captured from the responder; follow-ups, which go
// over REST, land on a local HTTP server.
type interactionHarness struct {
	t         *testing.T
	router    *handler.Mux
	client    *bot.Client
	followups chan string
}

type interactionResponse struct {
	typ  discord.InteractionResponseType
	data discord.InteractionResponseData
}

func newInteractionHarness(t *testing.T, m *fakeManager) *interactionHarness {
	t.Helper()
	followups := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg discord.MessageCreate
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Errorf("decode follow-up: %v", err)
		}
		followups <- msg.Content
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"1","channel_id":"1"}`)
	}))
	t.Cleanup(srv.Close)

	h := NewCommandHandlers(m, newTestBotMetrics(t), i18n.MustBundle())
	r := handler.New()
	h.Register(r)
	return &interactionHarness{
		t:      t,
		router: r,
		client: &bot.Client{
			Caches: cache.New(cache.WithCaches(cache.FlagsAll)),
			Logger: slog.New(slog.DiscardHandler),
			Rest:   rest.New(rest.NewClient("", rest.WithURL(srv.URL))),
		},
		followups: followups,
	}
}

func (h *interactionHarness) dispatch(payload string) interactionResponse {
	h.t.Helper()
	interaction, err := discord.UnmarshalInteraction([]byte(payload))
	if err != nil {
		h.t.Fatalf("unmarshal interaction: %v\n%s", err, payload)
	}
	var got []interactionResponse
	h.router.OnEvent(&events.InteractionCreate{
		GenericEvent: events.NewGenericEvent(h.client, 0, 0),
		Interaction:  interaction,
		Respond: func(typ discord.InteractionResponseType, data discord.InteractionResponseData, _ ...rest.RequestOpt) error {
			got = append(got, interactionResponse{typ, data})
			return nil
		},
	})
	if len(got) != 1 {
		h.t.Fatalf("want exactly one interaction response, got %d", len(got))
	}
	return got[0]
}

func (h *interactionHarness) followUp() string {
	h.t.Helper()
	select {
	case msg := <-h.followups:
		return msg
	case <-time.After(2 * time.Second):
		h.t.Fatal("no follow-up message was sent")
		return ""
	}
}

func (h *interactionHarness) noFollowUp() {
	h.t.Helper()
	select {
	case msg := <-h.followups:
		h.t.Fatalf("unexpected follow-up: %q", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func memberJSON(perms string) string {
	return fmt.Sprintf(`"guild_id":"%s","member":{"user":{"id":"5","username":"op"},"roles":[],"permissions":%q,"joined_at":"2024-01-01T00:00:00Z"}`, testGuildID, perms)
}

const dmUserJSON = `"user":{"id":"5","username":"op"}`

func slashPayload(who, name string, options ...string) string {
	return fmt.Sprintf(`{"id":"1","application_id":"2","type":2,"token":"tok","locale":"en-US",%s,
		"data":{"id":"3","name":%q,"type":1,"options":[%s]}}`, who, name, strings.Join(options, ","))
}

func stringOption(name, value string) string {
	return fmt.Sprintf(`{"name":%q,"type":3,"value":%q}`, name, value)
}

func componentPayload(who, customID string, componentType int, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"id":"1","application_id":"2","type":3,"token":"tok","locale":"en-US",%s,
		"message":{"id":"4","channel_id":"6","content":""},
		"data":{"custom_id":%q,"component_type":%d%s}}`, who, customID, componentType, extra)
}

func roleSelectData(roles ...snowflake.ID) string {
	values, resolved := make([]string, 0, len(roles)), make([]string, 0, len(roles))
	for _, r := range roles {
		values = append(values, fmt.Sprintf("%q", r.String()))
		resolved = append(resolved, fmt.Sprintf(`%q:{"id":%q,"name":"r","permissions":"0"}`, r.String(), r.String()))
	}
	return fmt.Sprintf(`"values":[%s],"resolved":{"roles":{%s}}`, strings.Join(values, ","), strings.Join(resolved, ","))
}

func channelSelectData(channels ...snowflake.ID) string {
	values, resolved := make([]string, 0, len(channels)), make([]string, 0, len(channels))
	for _, c := range channels {
		values = append(values, fmt.Sprintf("%q", c.String()))
		resolved = append(resolved, fmt.Sprintf(`%q:{"id":%q,"name":"voice","type":2,"permissions":"0","guild_id":%q}`, c.String(), c.String(), testGuildID.String()))
	}
	return fmt.Sprintf(`"values":[%s],"resolved":{"channels":{%s}}`, strings.Join(values, ","), strings.Join(resolved, ","))
}

func messageContent(t *testing.T, r interactionResponse) string {
	t.Helper()
	switch d := r.data.(type) {
	case discord.MessageCreate:
		return d.Content
	case discord.MessageUpdate:
		if d.Content == nil {
			return ""
		}
		return *d.Content
	default:
		t.Fatalf("response data is %T, want a message", r.data)
		return ""
	}
}

func wantEphemeral(t *testing.T, r interactionResponse, want string) {
	t.Helper()
	msg, ok := r.data.(discord.MessageCreate)
	if !ok {
		t.Fatalf("response data is %T, want MessageCreate", r.data)
	}
	if msg.Flags&discord.MessageFlagEphemeral == 0 {
		t.Errorf("response is not ephemeral: %q", msg.Content)
	}
	if msg.Content != want {
		t.Errorf("content = %q, want %q", msg.Content, want)
	}
}

func wantUpdate(t *testing.T, r interactionResponse, prefix string) discord.MessageUpdate {
	t.Helper()
	if r.typ != discord.InteractionResponseTypeUpdateMessage {
		t.Fatalf("response type = %v, want update message (content %q)", r.typ, messageContent(t, r))
	}
	upd := r.data.(discord.MessageUpdate)
	if upd.Content == nil || !strings.HasPrefix(*upd.Content, prefix) {
		t.Errorf("updated content = %v, want prefix %q", upd.Content, prefix)
	}
	return upd
}

func activeStatus(snowflake.ID) guild.Status {
	st := guild.NewStatus(testGuildID, 0)
	st.Session = &guild.Session{}
	return *st
}

func statusWithSpeakers(n int, enabled bool) func(snowflake.ID) guild.Status {
	return func(snowflake.ID) guild.Status {
		st := guild.NewStatus(testGuildID, 0)
		for i := range n {
			id := speakerA + snowflake.ID(i)
			st.Speakers[id] = &guild.Speaker{ID: id, Username: fmt.Sprintf("speaker-%d", i), Enabled: enabled}
		}
		return *st
	}
}

func TestCommandsOutsideAGuildAreRejected(t *testing.T) {
	t.Parallel()
	h := newInteractionHarness(t, &fakeManager{})
	want := en.T("err.guild_only")

	for name, payload := range map[string]string{
		"slash command": slashPayload(dmUserJSON, "status"),
		"button":        componentPayload(dmUserJSON, "/roles/menu", componentBtn, ""),
		"select menu":   componentPayload(dmUserJSON, "/setup/bind-role", componentRole, roleSelectData(selectedRole)),
	} {
		t.Run(name, func(t *testing.T) {
			wantEphemeral(t, h.dispatch(payload), want)
		})
	}
}

func TestCommandPermissions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		command, perms string
		denied         string
	}{
		{"setup", permManage, "err.need_admin"},
		{"setup", permAdmin, ""},
		{"start", permNone, "err.need_manager"},
		{"start", permManage, ""},
		{"stop", permNone, "err.need_manager"},
		{"status", permNone, ""},
	}
	for _, tt := range tests {
		t.Run(tt.command+"/"+tt.perms, func(t *testing.T) {
			t.Parallel()
			h := newInteractionHarness(t, &fakeManager{})
			resp := h.dispatch(slashPayload(memberJSON(tt.perms), tt.command))
			got := messageContent(t, resp)
			if tt.denied != "" {
				wantEphemeral(t, resp, en.T(tt.denied))
				return
			}
			if got == en.T("err.need_admin") || got == en.T("err.need_manager") {
				t.Errorf("authorized member was denied: %q", got)
			}
			if resp.typ == discord.InteractionResponseTypeDeferredCreateMessage {
				h.followUp()
			}
		})
	}
}

func TestSetupCommand(t *testing.T) {
	t.Parallel()
	t.Run("opens the main menu", func(t *testing.T) {
		h := newInteractionHarness(t, &fakeManager{})
		resp := h.dispatch(slashPayload(memberJSON(permAdmin), "setup"))
		msg := resp.data.(discord.MessageCreate)
		if !strings.HasPrefix(msg.Content, en.T("setup.main_title")) || len(msg.Components) == 0 {
			t.Errorf("setup message = %q with %d components, want the main menu", msg.Content, len(msg.Components))
		}
		if msg.Flags&discord.MessageFlagEphemeral == 0 {
			t.Error("setup menu must be ephemeral")
		}
	})
	t.Run("blocked during a raid", func(t *testing.T) {
		h := newInteractionHarness(t, &fakeManager{hasActiveFn: func(snowflake.ID) bool { return true }})
		wantEphemeral(t, h.dispatch(slashPayload(memberJSON(permAdmin), "setup")), en.T("setup.blocked_active_raid"))
	})
}

func TestStatusCommandRendersGuildStatus(t *testing.T) {
	t.Parallel()
	m := &fakeManager{statusFn: statusWithSpeakers(2, true)}
	h := newInteractionHarness(t, m)
	wantEphemeral(t, h.dispatch(slashPayload(memberJSON(permNone), "status")), m.GetStatus(testGuildID).Render(en))
}

func TestStartCommandMapsModeAndCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode, code string
		want       guild.RaidMode
	}{
		{"", "", guild.RaidModeOneCaller},
		{callerModeOne, "", guild.RaidModeOneCaller},
		{callerModeMany, "", guild.RaidModeGuildCaller},
		{callerModeOneMany, "", guild.RaidModeOneManyGuildCaller},
		{"", "CODE1234", guild.RaidModeAllyListener},
		{callerModeMany, "CODE1234", guild.RaidModeAllyCaller},
		{callerModeOneMany, "CODE1234", guild.RaidModeOneManyAllyCaller},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("mode=%q code=%q", tt.mode, tt.code), func(t *testing.T) {
			t.Parallel()
			m := &fakeManager{}
			h := newInteractionHarness(t, m)
			var opts []string
			if tt.mode != "" {
				opts = append(opts, stringOption("mode", tt.mode))
			}
			if tt.code != "" {
				opts = append(opts, stringOption("code", tt.code))
			}

			resp := h.dispatch(slashPayload(memberJSON(permManage), "start", opts...))
			if resp.typ != discord.InteractionResponseTypeDeferredCreateMessage {
				t.Fatalf("response type = %v, want deferred", resp.typ)
			}
			msg := h.followUp()

			m.mu.Lock()
			defer m.mu.Unlock()
			if tt.code == "" {
				if len(m.startModes) != 1 || m.startModes[0] != tt.want {
					t.Errorf("StartVoiceRaid modes = %v, want [%v]", m.startModes, tt.want)
				}
				if msg != en.T("raid.started") {
					t.Errorf("follow-up = %q, want started", msg)
				}
				return
			}
			if len(m.joinCodes) != 1 || m.joinCodes[0] != ally.Code(tt.code) {
				t.Errorf("JoinSession codes = %v, want [%s]", m.joinCodes, tt.code)
			}
			if want := en.T("raid.joined", "Code", tt.code, "Mode", tt.want.Pretty(en)); msg != want {
				t.Errorf("follow-up = %q, want %q", msg, want)
			}
		})
	}
}

func TestStartCommandFollowUps(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	warning := []manager.ChannelAccessWarning{{BotID: speakerA, ChannelID: boundChannel}}
	withCode := []string{stringOption("code", "CODE1234")}

	tests := []struct {
		name    string
		m       *fakeManager
		opts    []string
		want    string
		started bool
	}{
		{name: "relay code", m: &fakeManager{startFn: func(guild.RaidMode) (ally.Code, error) { return "ABCD2345", nil }},
			want: en.T("raid.started_with_code", "Code", "ABCD2345"), started: true},
		{name: "deafen unavailable", m: &fakeManager{deafenReadiness: manager.DeafenReadiness{MissingPermission: true}},
			want: en.T("raid.started") + "\n\n" + en.T("raid.deafen_unavailable"), started: true},
		{name: "skipped speakers", m: &fakeManager{missingFn: func() []manager.ChannelAccessWarning { return warning }},
			want: en.T("raid.started") + formatSkippedSpeakers(en, warning), started: true},
		{name: "access blocked", m: &fakeManager{accessFn: func() []manager.ChannelAccessWarning { return warning }},
			want: en.T("raid.start_blocked_permissions") + formatAccessWarnings(en, warning)},
		{name: "no bound speakers", m: &fakeManager{startFn: failStart(manager.ErrNoBoundSpeakers)}, want: en.T("raid.no_bound_speakers"), started: true},
		{name: "session exists", m: &fakeManager{startFn: failStart(manager.ErrSessionExists)}, want: en.T("raid.already_active"), started: true},
		{name: "no owner channel", m: &fakeManager{startFn: failStart(manager.ErrNoOwnerChannel)}, want: en.T("raid.no_owner_channel"), started: true},
		{name: "no caller role", m: &fakeManager{startFn: failStart(manager.ErrNoCallerRole)}, want: en.T("raid.no_caller_role"), started: true},
		{name: "other error", m: &fakeManager{startFn: failStart(boom)}, want: en.T("raid.start_failed", "Err", "boom"), started: true},

		{name: "join access blocked", opts: withCode, m: &fakeManager{accessFn: func() []manager.ChannelAccessWarning { return warning }},
			want: en.T("raid.join_blocked_permissions") + formatAccessWarnings(en, warning)},
		{name: "join no bound speakers", opts: withCode, m: &fakeManager{joinFn: failJoin(manager.ErrNoBoundSpeakers)}, want: en.T("raid.no_bound_speakers"), started: true},
		{name: "join no caller role", opts: withCode, m: &fakeManager{joinFn: failJoin(manager.ErrNoCallerRole)}, want: en.T("raid.no_caller_role"), started: true},
		{name: "join session exists", opts: withCode, m: &fakeManager{joinFn: failJoin(manager.ErrSessionExists)}, want: en.T("raid.already_active"), started: true},
		{name: "join other error", opts: withCode, m: &fakeManager{joinFn: failJoin(boom)}, want: en.T("raid.join_failed", "Code", "CODE1234", "Err", "boom"), started: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newInteractionHarness(t, tt.m)
			h.dispatch(slashPayload(memberJSON(permManage), "start", tt.opts...))
			if got := h.followUp(); got != tt.want {
				t.Errorf("follow-up = %q, want %q", got, tt.want)
			}
			tt.m.mu.Lock()
			defer tt.m.mu.Unlock()
			if attempted := len(tt.m.startModes)+len(tt.m.joinCodes) > 0; attempted != tt.started {
				t.Errorf("raid attempted = %v, want %v", attempted, tt.started)
			}
		})
	}
}

func failStart(err error) func(guild.RaidMode) (ally.Code, error) {
	return func(guild.RaidMode) (ally.Code, error) { return "", err }
}

func failJoin(err error) func(guild.RaidMode, ally.Code) (guild.RaidMode, error) {
	return func(guild.RaidMode, ally.Code) (guild.RaidMode, error) { return "", err }
}

func TestStartCommandWhileRaidActive(t *testing.T) {
	t.Parallel()
	m := &fakeManager{statusFn: activeStatus}
	h := newInteractionHarness(t, m)
	wantEphemeral(t, h.dispatch(slashPayload(memberJSON(permManage), "start")), en.T("raid.already_active"))
	h.noFollowUp()
	if len(m.startModes) != 0 {
		t.Errorf("StartVoiceRaid called %d times, want 0", len(m.startModes))
	}
}

func TestStopCommandFollowUps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"stopped", nil, en.T("raid.stopped")},
		{"none active", manager.ErrNoActiveSession, en.T("raid.none_active")},
		{"failed", errors.New("boom"), en.T("raid.stop_failed", "Err", "boom")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newInteractionHarness(t, &fakeManager{stopFn: func() error { return tt.err }})
			resp := h.dispatch(slashPayload(memberJSON(permManage), "stop"))
			if resp.typ != discord.InteractionResponseTypeDeferredCreateMessage {
				t.Fatalf("response type = %v, want deferred", resp.typ)
			}
			if got := h.followUp(); got != tt.want {
				t.Errorf("follow-up = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetupNavigation(t *testing.T) {
	t.Parallel()
	h := newInteractionHarness(t, &fakeManager{statusFn: statusWithSpeakers(1, true)})
	who := memberJSON(permAdmin)

	wantUpdate(t, h.dispatch(componentPayload(who, "/roles/menu", componentBtn, "")), en.T("setup.roles_title"))
	wantUpdate(t, h.dispatch(componentPayload(who, "/speakers/menu", componentBtn, "")), en.T("setup.main_title"))
	wantUpdate(t, h.dispatch(componentPayload(who, "/speakers/page/0", componentBtn, "")),
		en.T("setup.speakers_title", "Page", 1, "Total", 1))
	// A malformed page falls back to the first page instead of failing.
	wantUpdate(t, h.dispatch(componentPayload(who, "/speakers/page/x", componentBtn, "")),
		en.T("setup.speakers_title", "Page", 1, "Total", 1))
}

func TestRoleMenuBindings(t *testing.T) {
	t.Parallel()
	who := memberJSON(permAdmin)
	tests := []struct {
		name, customID, data string
		want                 bindCall
	}{
		{"bind caller", "/setup/bind-role", roleSelectData(selectedRole), bindCall{"bind-role", "caller", selectedRole.String()}},
		{"unbind caller", "/setup/bind-role", roleSelectData(), bindCall{"unbind-role", "caller", ""}},
		{"bind manager", "/setup/bind-manager-role", roleSelectData(selectedRole), bindCall{"bind-role", "manager", selectedRole.String()}},
		{"unbind manager", "/setup/bind-manager-role", roleSelectData(), bindCall{"unbind-role", "manager", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &fakeManager{}
			h := newInteractionHarness(t, m)
			wantUpdate(t, h.dispatch(componentPayload(who, tt.customID, componentRole, tt.data)), en.T("setup.roles_title"))
			if len(m.bindings) != 1 || m.bindings[0] != tt.want {
				t.Errorf("bindings = %v, want [%v]", m.bindings, tt.want)
			}
		})
	}
}

func TestCallerRoleCannotChangeDuringRaid(t *testing.T) {
	t.Parallel()
	m := &fakeManager{hasActiveFn: func(snowflake.ID) bool { return true }}
	h := newInteractionHarness(t, m)
	resp := h.dispatch(componentPayload(memberJSON(permAdmin), "/setup/bind-role", componentRole, roleSelectData(selectedRole)))
	wantEphemeral(t, resp, en.T("setup.blocked_active_raid"))
	if len(m.bindings) != 0 {
		t.Errorf("bindings = %v, want none", m.bindings)
	}
}

func TestSelectMenusRejectUnexpectedDataType(t *testing.T) {
	t.Parallel()
	who := memberJSON(permAdmin)
	textData := `"values":["x"]`
	channelData := channelSelectData(boundChannel)
	for name, payload := range map[string]string{
		"role menu":     componentPayload(who, "/setup/bind-role", componentText, textData),
		"locale":        componentPayload(who, "/setup/locale", componentChan, channelData),
		"owner channel": componentPayload(who, "/owner/bind-channel", componentText, textData),
		"speaker":       componentPayload(who, fmt.Sprintf("/speakers/bind-channel/%s/0", speakerA), componentText, textData),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := &fakeManager{}
			h := newInteractionHarness(t, m)
			wantEphemeral(t, h.dispatch(payload), en.T("err.unexpected_data_type"))
			if len(m.bindings) != 0 {
				t.Errorf("bindings = %v, want none", m.bindings)
			}
		})
	}
}

func TestLocaleMenuRedrawsInNewLocale(t *testing.T) {
	t.Parallel()
	m := &fakeManager{}
	h := newInteractionHarness(t, m)
	who := memberJSON(permAdmin)
	de := i18n.MustBundle().For("de", discord.LocaleEnglishUS)

	wantUpdate(t, h.dispatch(componentPayload(who, "/setup/locale", componentText, `"values":["de"]`)), de.T("setup.main_title"))
	wantUpdate(t, h.dispatch(componentPayload(who, "/setup/locale", componentText, fmt.Sprintf(`"values":[%q]`, localeAutoValue))), en.T("setup.main_title"))

	want := []bindCall{{"bind-locale", "", "de"}, {"unbind-locale", "", ""}}
	if fmt.Sprint(m.bindings) != fmt.Sprint(want) {
		t.Errorf("bindings = %v, want %v", m.bindings, want)
	}
}

func TestChannelMenuBindings(t *testing.T) {
	t.Parallel()
	who := memberJSON(permAdmin)
	speakerPath := fmt.Sprintf("/speakers/bind-channel/%s/0", speakerA)
	speakersTitle := en.T("setup.speakers_title", "Page", 1, "Total", 1)
	tests := []struct {
		name, customID, data, title string
		want                        bindCall
	}{
		{"bind owner", "/owner/bind-channel", channelSelectData(boundChannel), en.T("setup.main_title"),
			bindCall{"bind-channel", fakeOwnerBotID.String(), boundChannel.String()}},
		{"unbind owner", "/owner/bind-channel", channelSelectData(), en.T("setup.main_title"),
			bindCall{"unbind-channel", fakeOwnerBotID.String(), ""}},
		{"bind speaker", speakerPath, channelSelectData(boundChannel), speakersTitle,
			bindCall{"bind-channel", speakerA.String(), boundChannel.String()}},
		{"unbind speaker", speakerPath, channelSelectData(), speakersTitle,
			bindCall{"unbind-channel", speakerA.String(), ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &fakeManager{statusFn: statusWithSpeakers(1, true)}
			h := newInteractionHarness(t, m)
			wantUpdate(t, h.dispatch(componentPayload(who, tt.customID, componentChan, tt.data)), tt.title)
			if len(m.bindings) != 1 || m.bindings[0] != tt.want {
				t.Errorf("bindings = %v, want [%v]", m.bindings, tt.want)
			}
		})
	}
}

func TestSpeakerIDMustParse(t *testing.T) {
	t.Parallel()
	h := newInteractionHarness(t, &fakeManager{})
	who := memberJSON(permAdmin)
	want := en.T("err.invalid_speaker_id")
	wantEphemeral(t, h.dispatch(componentPayload(who, "/speakers/toggle/nope/0", componentBtn, "")), want)
	wantEphemeral(t, h.dispatch(componentPayload(who, "/speakers/bind-channel/nope/0", componentChan, channelSelectData(boundChannel))), want)
}

func TestToggleSpeaker(t *testing.T) {
	t.Parallel()
	who := memberJSON(permAdmin)
	toggle := componentPayload(who, fmt.Sprintf("/speakers/toggle/%s/0", speakerA), componentBtn, "")

	t.Run("flips the enabled state", func(t *testing.T) {
		t.Parallel()
		for _, enabled := range []bool{true, false} {
			m := &fakeManager{statusFn: statusWithSpeakers(1, enabled)}
			h := newInteractionHarness(t, m)
			wantUpdate(t, h.dispatch(toggle), en.T("setup.speakers_title", "Page", 1, "Total", 1))
			if want := (toggleCall{speakerA, !enabled}); len(m.toggleCalls) != 1 || m.toggleCalls[0] != want {
				t.Errorf("toggle calls = %v, want [%v]", m.toggleCalls, want)
			}
		}
	})
	t.Run("unknown speaker", func(t *testing.T) {
		t.Parallel()
		m := &fakeManager{statusFn: statusWithSpeakers(0, true)}
		h := newInteractionHarness(t, m)
		wantEphemeral(t, h.dispatch(toggle), en.T("err.speaker_not_found"))
		if len(m.toggleCalls) != 0 {
			t.Errorf("toggle calls = %v, want none", m.toggleCalls)
		}
	})
	t.Run("manager refuses", func(t *testing.T) {
		t.Parallel()
		m := &fakeManager{
			statusFn: statusWithSpeakers(1, true),
			toggleFn: func(snowflake.ID, bool) error { return errors.New("raid active") },
		}
		h := newInteractionHarness(t, m)
		wantEphemeral(t, h.dispatch(toggle), "❌ raid active")
	})
}

func TestAddSpeakerButton(t *testing.T) {
	t.Parallel()
	who := memberJSON(permAdmin)
	add := componentPayload(who, "/speakers/add", componentBtn, "")

	t.Run("pool exhausted", func(t *testing.T) {
		t.Parallel()
		h := newInteractionHarness(t, &fakeManager{})
		wantEphemeral(t, h.dispatch(add), en.T("err.no_more_tokens"))
	})
	t.Run("invites the next bot to this guild", func(t *testing.T) {
		t.Parallel()
		next := snowflake.ID(4242)
		h := newInteractionHarness(t, &fakeManager{nextSpeakerFn: func() (snowflake.ID, bool) { return next, true }})
		upd := wantUpdate(t, h.dispatch(add), en.T("speaker.add_title"))
		invite := (*upd.Components)[0].(discord.ActionRowComponent).Components[0].(discord.ButtonComponent)
		if invite.URL != installURL(next, testGuildID) {
			t.Errorf("invite URL = %q, want %q", invite.URL, installURL(next, testGuildID))
		}
	})
}

func TestSpeakersPagePagination(t *testing.T) {
	t.Parallel()
	navLabels := func(components []discord.LayoutComponent) (labels []string, current string) {
		row := components[len(components)-1].(discord.ActionRowComponent)
		for _, c := range row.Components[1:] {
			b := c.(discord.ButtonComponent)
			labels = append(labels, b.Label)
			if b.Disabled {
				current = b.Label
			}
		}
		return labels, current
	}

	tests := []struct {
		name         string
		speakers     int
		page         int
		wantLabels   []string
		wantCurrent  string
		wantSpeakers int
	}{
		{"no speakers", 0, 0, nil, "", 0},
		{"partial last page", 4, 1, []string{"1-3", "4-4"}, "4-4", 1},
		{"window at start", 15, 0, []string{"1-3", "4-6", "7-9", "10-12"}, "1-3", 3},
		{"window centred", 15, 2, []string{"1-3", "4-6", "7-9", "10-12"}, "7-9", 3},
		{"window clamped at end", 15, 4, []string{"4-6", "7-9", "10-12", "13-15"}, "13-15", 3},
		{"page beyond the end", 4, 9, []string{"1-3", "4-4"}, "4-4", 1},
		{"negative page", 4, -1, []string{"1-3", "4-4"}, "1-3", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &CommandHandlers{manager: &fakeManager{statusFn: statusWithSpeakers(tt.speakers, true)}}
			content, components := h.buildSpeakersPageMessage(testGuildID, en, tt.page)

			labels, current := navLabels(components)
			if tt.speakers > 0 && (fmt.Sprint(labels) != fmt.Sprint(tt.wantLabels) || current != tt.wantCurrent) {
				t.Errorf("nav = %v (current %q), want %v (current %q)", labels, current, tt.wantLabels, tt.wantCurrent)
			}
			// One toggle row plus one channel menu per speaker on the page, then the nav row.
			wantRows := 1
			if tt.wantSpeakers > 0 {
				wantRows += 1 + tt.wantSpeakers
			}
			if len(components) != wantRows {
				t.Errorf("rows = %d, want %d", len(components), wantRows)
			}
			if tt.speakers == 0 && !strings.Contains(content, en.T("setup.no_speakers")) {
				t.Errorf("content = %q, want the no-speakers notice", content)
			}
		})
	}
}
