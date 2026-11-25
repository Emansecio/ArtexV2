package transcript

import "context"

type ctxKey int

const (
	sessionIDKey ctxKey = iota
	// sessionHeaderIDKey carries an optional override for the provider-facing
	// session header, so a forked sidechain can pin the parent's cache shard
	// while keeping its own transcript/llmrec session id.
	sessionHeaderIDKey
)

// WithSessionID carries the active session id on the context so subagents can
// write their sidechain under the parent session's directory.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey, sessionID)
}

// SessionIDFrom returns the session id carried on ctx, or "" if none.
func SessionIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(sessionIDKey).(string)
	return v
}

// WithSessionHeaderID overrides the session id sent on provider HTTP headers
// (e.g. session-sticky prompt-cache routing) without changing the transcript
// session id. "" clears any override inherited from the parent.
func WithSessionHeaderID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionHeaderIDKey, sessionID)
}

// SessionHeaderIDFrom returns the header override, falling back to the
// transcript session id when no override is set.
func SessionHeaderIDFrom(ctx context.Context) string {
	if v, _ := ctx.Value(sessionHeaderIDKey).(string); v != "" {
		return v
	}
	return SessionIDFrom(ctx)
}
