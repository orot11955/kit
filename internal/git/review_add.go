package git

import "context"

// ResetHard restores the checked-out branch to an already validated commit.
// Callers use this only for transactional rollback after they recorded the
// original branch tip themselves.
func (s Service) ResetHard(ctx context.Context, revision string) error {
	if err := s.VerifyRevision(ctx, revision); err != nil {
		return err
	}
	_, err := s.run(ctx, "reset", "--hard", revision)
	return err
}
