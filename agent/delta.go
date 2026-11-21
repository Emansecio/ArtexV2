package agent

import "context"

// DeltaEvent is an incremental assistant text/thinking fragment emitted during
// a streaming LLM turn, before the full step is finalized and persisted.
type DeltaEvent struct {
	Kind string `json:"kind"` // "text" | "thinking"
	Text string `json:"text"`
}

type deltaSinkKey struct{}

// WithDeltaSink attaches an in-memory live delta callback to ctx. captureRun
// and captureRunSession coalesce streamed tokens and push them through this
// sink immediately without waiting for Postgres persistence.
func WithDeltaSink(ctx context.Context, fn func(DeltaEvent)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, deltaSinkKey{}, fn)
}

func deltaSinkFrom(ctx context.Context) func(DeltaEvent) {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(deltaSinkKey{}).(func(DeltaEvent))
	return fn
}
