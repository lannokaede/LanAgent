package tool

import "context"

type executionKey struct{}

// Execution metadata is supplied by the host, never by model arguments.
func WithExecutionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, executionKey{}, id)
}
func ExecutionID(ctx context.Context) string {
	id, _ := ctx.Value(executionKey{}).(string)
	return id
}
