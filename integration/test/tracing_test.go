//go:build integration || stress

package test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestJoinPrinterShowsWhereTheHandshakeStopped(t *testing.T) {
	var out bytes.Buffer
	tracer := trace.NewTracerProvider(trace.WithSpanProcessor(newJoinPrinter(&out))).Tracer("tracing_test")

	_, failed := tracer.Start(context.Background(), "voice.conn.open",
		oteltrace.WithAttributes(attribute.String("bot.kind", "harness-source")))
	failed.AddEvent("voice.state.update", oteltrace.WithAttributes(attribute.Bool("voice.channel.joined", true)))
	failed.AddEvent("voice.server.update", oteltrace.WithAttributes(attribute.Bool("voice.endpoint.assigned", false)))
	failed.RecordError(errors.New("context deadline exceeded"))
	failed.SetStatus(codes.Error, "context deadline exceeded")
	failed.End()

	_, hung := tracer.Start(context.Background(), "voice.conn.open")
	hung.SetStatus(codes.Error, "context deadline exceeded")
	hung.End()

	_, ok := tracer.Start(context.Background(), "voice.conn.open")
	ok.AddEvent("voice.session.description")
	ok.End()

	_, retried := tracer.Start(context.Background(), "voice.conn.open")
	retried.AddEvent("voice.state.update")
	retried.AddEvent("voice.join.retry")
	retried.AddEvent("voice.session.description")
	retried.End()

	_, other := tracer.Start(context.Background(), "voice.bot.attach")
	other.SetStatus(codes.Error, "context deadline exceeded")
	other.End()

	got := out.String()
	for _, want := range []string{
		"voice join failed after",
		"bot.kind=harness-source",
		"voice.state.update voice.channel.joined=true",
		"voice.server.update voice.endpoint.assigned=false",
		"no handshake step reached",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "voice join failed"); n != 2 {
		t.Errorf("reported %d failed joins, want 2 (only failed voice.conn.open spans):\n%s", n, got)
	}
	if n := strings.Count(got, "voice join retried"); n != 1 {
		t.Errorf("reported %d retried joins, want 1 (a join that hung once and then succeeded):\n%s", n, got)
	}
	if strings.Contains(got, "exception") {
		t.Errorf("output repeats the recorded error as a step:\n%s", got)
	}
	t.Logf("output:\n%s", got)
}
