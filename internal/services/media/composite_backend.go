package media

import "context"

type SIPBackend interface {
	StreamStart(ctx context.Context, devAddr string) error
	StreamStop(ctx context.Context) error
}

type StreamCommandBackend interface {
	StreamStart(ctx context.Context, audioPort, videoPort int) error
}

type compositeBackend struct {
	sip       SIPBackend
	commands  StreamCommandBackend
	audioPort int
	videoPort int
}

func NewCompositeBackend(sip SIPBackend, commands StreamCommandBackend, audioPort, videoPort int) Backend {
	return &compositeBackend{
		sip:       sip,
		commands:  commands,
		audioPort: audioPort,
		videoPort: videoPort,
	}
}

func (b *compositeBackend) StreamStart(ctx context.Context, devAddr string) error {
	if b.sip != nil {
		if err := b.sip.StreamStart(ctx, devAddr); err != nil {
			return err
		}
		return nil
	}
	if b.commands != nil {
		if err := b.commands.StreamStart(ctx, b.audioPort, b.videoPort); err != nil {
			return err
		}
	}
	return nil
}

func (b *compositeBackend) StreamStop(ctx context.Context) error {
	if b.sip != nil {
		return b.sip.StreamStop(ctx)
	}
	return nil
}
