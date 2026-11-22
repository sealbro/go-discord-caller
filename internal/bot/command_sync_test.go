package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/sealbro/go-discord-caller/internal/i18n"
)

const syncTestAppID = snowflake.ID(700)

// commandPuts records how many commands each command-set PUT carried, keyed by
// the request path.
type commandPuts struct {
	mu   sync.Mutex
	sent map[string]int
}

func (c *commandPuts) get(path string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.sent[path]
	return n, ok
}

func newCommandSyncBot(t *testing.T) (*Bot, *commandPuts) {
	t.Helper()
	puts := &commandPuts{sent: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var commands []json.RawMessage
		if err := json.Unmarshal(body, &commands); err != nil {
			t.Errorf("decode %s %s: %v", r.Method, r.URL.Path, err)
		}
		puts.mu.Lock()
		puts.sent[r.Method+" "+r.URL.Path] = len(commands)
		puts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, "[]")
	}))
	t.Cleanup(srv.Close)

	return &Bot{
		client: &bot.Client{
			ApplicationID: syncTestAppID,
			Logger:        slog.New(slog.DiscardHandler),
			Rest:          rest.New(rest.NewClient("", rest.WithURL(srv.URL))),
		},
		bundle: i18n.MustBundle(),
	}, puts
}

// The app directory lists only global commands, so the full set is registered
// globally and every guild's own set is emptied; a guild copy left behind
// would show each command twice in that guild.
func TestSyncCommandsRegistersGloballyAndClearsGuilds(t *testing.T) {
	t.Parallel()
	b, puts := newCommandSyncBot(t)
	want := len(BuildCommands(i18n.MustBundle()))

	b.syncCommands(context.Background(), []snowflake.ID{701, 702})

	global := fmt.Sprintf("PUT /applications/%s/commands", syncTestAppID)
	if n, ok := puts.get(global); !ok || n != want {
		t.Errorf("global commands: sent=%v count=%d, want %d", ok, n, want)
	}
	for _, gid := range []snowflake.ID{701, 702} {
		path := fmt.Sprintf("PUT /applications/%s/guilds/%s/commands", syncTestAppID, gid)
		if n, ok := puts.get(path); !ok || n != 0 {
			t.Errorf("guild %s commands: sent=%v count=%d, want an empty set", gid, ok, n)
		}
	}
}

// A global command without contexts also shows in the bot's DMs, where every
// command needs a guild and fails.
func TestCommandsAreGuildOnly(t *testing.T) {
	t.Parallel()
	for _, c := range BuildCommands(i18n.MustBundle()) {
		slash, ok := c.(discord.SlashCommandCreate)
		if !ok {
			t.Fatalf("%s: unexpected command type %T", c.CommandName(), c)
		}
		if len(slash.Contexts) != 1 || slash.Contexts[0] != discord.InteractionContextTypeGuild {
			t.Errorf("/%s contexts = %v, want guild only", slash.Name, slash.Contexts)
		}
	}
}

// A guild that removed the bot before commands went global keeps its old guild
// copies; joining again must clear them rather than add another full set.
func TestSyncGuildCommandsClearsTheGuildSet(t *testing.T) {
	t.Parallel()
	b, puts := newCommandSyncBot(t)

	b.syncGuildCommands(context.Background(), 703)

	path := fmt.Sprintf("PUT /applications/%s/guilds/%s/commands", syncTestAppID, snowflake.ID(703))
	if n, ok := puts.get(path); !ok || n != 0 {
		t.Errorf("guild commands: sent=%v count=%d, want an empty set", ok, n)
	}
}
