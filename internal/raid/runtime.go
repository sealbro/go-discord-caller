// Package raid builds and runs the audio of voice raids: the pipelines that
// wire captured sources to speakers, and the router that switches them.
package raid

import (
	"context"

	"github.com/sealbro/go-discord-caller/internal/guild"
	"github.com/sealbro/go-discord-caller/internal/raid/pipeline"
)

// Runtime builds the audio graph of a raid. The service decides who joins and
// when a raid starts and stops; everything between captured frames and the
// speakers' output belongs to the runtime.
type Runtime interface {
	BuildHost(ctx context.Context, p pipeline.Params) (session *guild.Session, start func(), err error)
	BuildGuest(ctx context.Context, p pipeline.GuestParams) (session *guild.Session, start func(), cleanup func(), err error)
}

// Local runs raid audio in this process.
type Local struct{}

func (Local) BuildHost(ctx context.Context, p pipeline.Params) (*guild.Session, func(), error) {
	return pipeline.HostFor(p.Mode).Build(ctx, p)
}

func (Local) BuildGuest(ctx context.Context, p pipeline.GuestParams) (*guild.Session, func(), func(), error) {
	return pipeline.GuestFor(p.GuestMode).Build(ctx, p)
}
