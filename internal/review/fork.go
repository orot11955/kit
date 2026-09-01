package review

import "context"

// ForkClient is an optional provider capability for creating or reusing a
// review whose source branch lives in a fork of the target repository.
// Same-repository review flows continue to use Client/OpenFinder unchanged.
type ForkClient interface {
	CreateFrom(ctx context.Context, sourceOwner string, request CreateRequest) (Review, error)
	FindOpenFrom(ctx context.Context, sourceOwner, sourceBranch, targetBranch string) (Review, bool, error)
}
