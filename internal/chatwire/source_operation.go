package chatwire

import "context"

type sourceOperationKey struct{}

func WithSourceOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, sourceOperationKey{}, operation)
}

func SourceOperation(ctx context.Context) string {
	operation, _ := ctx.Value(sourceOperationKey{}).(string)
	return operation
}
