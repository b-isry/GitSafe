// Package providers implements the GitHub integration client used by GitSafe.
//
// It hides all GitHub HTTP API details from the rest of the application. The
// backup system, state layer, and UI depend only on this package's types and
// the Client interface.
//
// The access token is supplied at construction time by the caller (resolved
// from the token store). It is never exposed through the client's public API
// and never appears in returned types.
package providers

import (
	"context"
	"time"
)

// Identity is the authenticated GitHub account, returned after OAuth succeeds.
type Identity struct {
	// ID is the canonical, immutable GitHub numeric user ID.
	ID int64
	// Login is the GitHub username.
	Login string
	// Name is the user's display name, if any.
	Name string
	// AvatarURL is the user's avatar URL, if any.
	AvatarURL string
	// Scopes are the OAuth scopes granted to the token.
	Scopes []string
}

// Repository is a GitHub repository discovered for the authenticated account.
//
// IMPORTANT: a repository is identified by its immutable GitHub numeric ID and
// addressed by its current human-readable fullName (owner/name). A local
// filesystem path is NEVER part of this model. cloneURL is the public clone URL
// WITHOUT credentials.
type Repository struct {
	// ID is the immutable GitHub numeric repository ID.
	ID int64
	// FullName is the current owner/name address.
	FullName string
	// Owner is the repository owner login.
	Owner string
	// Name is the repository name (without owner).
	Name string
	// Private reports whether the repository is private.
	Private bool
	// Fork reports whether the repository is a fork.
	Fork bool
	// Archived reports whether the repository has been archived.
	Archived bool
	// DefaultBranch is the repository's default branch name.
	DefaultBranch string
	// CloneURL is the public HTTPS clone URL, without credentials.
	CloneURL string
	// SSHURL is the SSH clone URL, if available.
	SSHURL string
	// Description is the repository description, if any.
	Description string
	// UpdatedAt is the last time the repository metadata was updated.
	UpdatedAt time.Time
	// PushedAt is the last time a commit was pushed to the repository.
	PushedAt time.Time
	// SizeKB is the repository size in kilobytes.
	SizeKB int64
}

// Client is the application-facing GitHub interface. It exposes only what the
// rest of GitSafe needs; HTTP specifics are internal.
type Client interface {
	// Identity returns the authenticated account for the token.
	Identity(ctx context.Context) (Identity, error)
	// ListRepositories returns all repositories accessible to the account,
	// walking every page until retrieval is complete.
	ListRepositories(ctx context.Context) ([]Repository, error)
	// Repository returns a single repository by its owner/name address.
	Repository(ctx context.Context, fullName string) (Repository, error)
}
