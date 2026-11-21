package pool

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"
)

const frameTime = voice.OpusFrameSizeMs * time.Millisecond

// sessionOf makes every conn of a voice manager use session.
func sessionOf(session godave.Session) godave.SessionCreateFunc {
	return func(*slog.Logger, godave.UserID, godave.Callbacks) godave.Session { return session }
}

// newBareVoiceManager builds disgo's voice manager without this package's conn
// wiring, as an upstream tripwire needs it.
func newBareVoiceManager(botID snowflake.ID, session godave.SessionCreateFunc, opts ...voice.ManagerConfigOpt) voice.Manager {
	return voice.NewManager(
		func(context.Context, snowflake.ID, *snowflake.ID, bool, bool) error { return nil },
		botID,
		append([]voice.ManagerConfigOpt{
			voice.WithDaveSessionCreateFunc(session),
			voice.WithLogger(slog.New(slog.DiscardHandler)),
		}, opts...)...,
	)
}

// newVoiceManager builds a voice manager wired as production wires every client.
func newVoiceManager(botID snowflake.ID, session godave.SessionCreateFunc, opts ...voice.ManagerConfigOpt) voice.Manager {
	return newBareVoiceManager(botID, session, append([]voice.ManagerConfigOpt{SafeAudioSenderOpt(), VoiceConnOpt()}, opts...)...)
}

func trustingDialer(srv *httptest.Server) *websocket.Dialer {
	return &websocket.Dialer{TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()}
}

func withDialer(d *websocket.Dialer) voice.ManagerConfigOpt {
	return voice.WithConnConfigOpts(voice.WithConnGatewayConfigOpts(voice.WithGatewayDialer(d)))
}

// openVoiceConn creates the guild's conn on vm and opens its gateway against srv.
func openVoiceConn(t *testing.T, vm voice.Manager, srv *httptest.Server, guildID, botID, channelID snowflake.ID) voice.Conn {
	t.Helper()

	conn := vm.CreateConn(guildID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Gateway().Open(ctx, voice.State{
		GuildID:   guildID,
		UserID:    botID,
		ChannelID: channelID,
		SessionID: "session",
		Token:     "token",
		Endpoint:  strings.TrimPrefix(srv.URL, "https://"),
	}); err != nil {
		t.Fatalf("open voice gateway: %v", err)
	}
	return conn
}

// stopInBubble closes an audio sender or receiver and lets its goroutine, parked
// in disgo's 20 ms sleep, wake to the cancellation and exit, which a synctest
// bubble needs before it can end.
func stopInBubble(s interface{ Close() }) {
	s.Close()
	time.Sleep(frameTime)
}

// eventually polls cond in real time and reports whether it held within timeout.
func eventually(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}
