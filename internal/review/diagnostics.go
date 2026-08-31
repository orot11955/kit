package review

import "context"

// Pinger is an optional capability used by network diagnostics. It performs a
// read-only authenticated request against the configured repository.
type Pinger interface {
	Ping(ctx context.Context) error
}
