//go:build integration || stress

package test

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/pool"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"github.com/sealbro/go-discord-caller/internal/telemetry/spantest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// InstallJoinTracer makes every voice join in the process traced, and writes
// each one that fails or needed a retry to w, together with the handshake
// steps it went through. The test guild hangs on a join now and then, as
// production does, and the steps are what tell a voice server Discord never
// assigned from a voice gateway or UDP discovery that never completed. Call it
// before the harness is built.
//
// The printer joins spantest's provider rather than installing its own: only
// the first provider installed reaches telemetry.Tracer, and tests that read
// spans through spantest (E5g) would otherwise record nothing.
func InstallJoinTracer(w io.Writer) {
	spantest.Record()
	tp, ok := otel.GetTracerProvider().(*trace.TracerProvider)
	if !ok {
		panic(fmt.Sprintf("spantest installed a %T, not an SDK tracer provider", otel.GetTracerProvider()))
	}
	tp.RegisterSpanProcessor(newJoinPrinter(w))
}

type joinPrinter struct {
	mu sync.Mutex
	w  io.Writer
}

func newJoinPrinter(w io.Writer) *joinPrinter { return &joinPrinter{w: w} }

func (p *joinPrinter) OnStart(context.Context, trace.ReadWriteSpan) {}
func (p *joinPrinter) Shutdown(context.Context) error               { return nil }
func (p *joinPrinter) ForceFlush(context.Context) error             { return nil }

func (p *joinPrinter) OnEnd(s trace.ReadOnlySpan) {
	if s.Name() != "voice.conn.open" {
		return
	}
	failed := s.Status().Code == codes.Error
	if !failed && !slices.ContainsFunc(s.Events(), func(e trace.Event) bool { return e.Name == "voice.join.retry" }) {
		return
	}

	var b strings.Builder
	took := s.EndTime().Sub(s.StartTime()).Round(time.Millisecond)
	if failed {
		fmt.Fprintf(&b, "voice join failed after %s (%s):", took, s.Status().Description)
	} else {
		fmt.Fprintf(&b, "voice join retried, joined after %s:", took)
	}
	for _, a := range s.Attributes() {
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value.Emit())
	}
	b.WriteByte('\n')
	if len(s.Events()) == 0 {
		b.WriteString("  no handshake step reached\n")
	}
	for _, e := range s.Events() {
		if e.Name == "exception" {
			continue
		}
		fmt.Fprintf(&b, "  +%s %s", e.Time.Sub(s.StartTime()).Round(time.Millisecond), e.Name)
		for _, a := range e.Attributes {
			fmt.Fprintf(&b, " %s=%s", a.Key, a.Value.Emit())
		}
		b.WriteByte('\n')
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = io.WriteString(p.w, b.String())
}

// joinTraced joins gv's channel inside a voice.conn.open span, as the raid
// path does for the production bots, so a harness bot's failed join is
// reported the same way.
func joinTraced(ctx context.Context, gv pool.GuildVoice, guildID, botID snowflake.ID, kind string) (voice.Conn, error) {
	ctx, span := telemetry.Tracer.Start(ctx, "voice.conn.open", oteltrace.WithAttributes(
		attribute.String("bot.id", botID.String()),
		attribute.String("bot.kind", kind),
	))
	defer span.End()

	conn, err := gv.ForBot(botID).Join(ctx, guildID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return conn, err
}
