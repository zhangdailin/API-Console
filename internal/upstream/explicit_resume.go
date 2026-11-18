package upstream

import "context"

type explicitResumeKey struct{}

// WithExplicitResume records whether the caller explicitly requested an upstream
// continuation. It does not grant authentication or permission to use a model.
func WithExplicitResume(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, explicitResumeKey{}, enabled)
}

// ExplicitResume reports the caller's opt-in, defaulting to false.
func ExplicitResume(ctx context.Context) bool {
	enabled, _ := ctx.Value(explicitResumeKey{}).(bool)
	return enabled
}
