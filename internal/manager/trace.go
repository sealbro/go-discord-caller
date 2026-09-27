package manager

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
	"github.com/sealbro/go-discord-caller/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// startPhase opens a child span for one step of session startup and returns the
// func that ends it, recording err when non-nil.
//
// Session startup is a chain of Discord round trips — voice handshake, member
// chunk request, speaking op, deafen PATCH — that takes seconds in production
// and used to be bracketed by nothing but two log lines. The phase spans are
// the only way to tell which round trip dominates, and which of them run
// concurrently rather than one after another.
//
// The returned context parents nested phases. It must not be handed to
// anything that outlives the phase — telemetry.Metrics.ForGuild captures its
// context for the whole session, so it keeps taking the session context.
func startPhase(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	ctx, span := telemetry.Tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// botAttr labels a phase with the bot whose connection it belongs to, so the
// owner's phases can be told apart from each speaker's.
func botAttr(botID snowflake.ID) attribute.KeyValue {
	return attribute.String("bot.id", botID.String())
}
