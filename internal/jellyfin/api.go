package jellyfin

import "context"

// API is everything the program asks of a Jellyfin server. Client is the real
// one; tests build fakes on jellyfintest.Unimplemented.
type API interface {
	// Check confirms the server is reachable and the token works.
	Check(ctx context.Context) error
	// Refresh asks for a library scan.
	Refresh(ctx context.Context) error
	// Authenticate checks a login, and returns whether the user is an
	// administrator and the session token it opened.
	Authenticate(ctx context.Context, username, password string) (admin bool, token string, err error)
	// EndSession closes a session opened by Authenticate.
	EndSession(ctx context.Context, token string) error
	// Username returns who a session token belongs to.
	Username(ctx context.Context, token string) (string, error)
	// ItemPath returns the file behind a library item.
	ItemPath(ctx context.Context, itemID string) (string, error)
	// ClearPlayed unmarks an item as played for one user.
	ClearPlayed(ctx context.Context, itemID, userID string) error
	// SendMessage shows a message in one client session.
	SendMessage(ctx context.Context, sessionID, header, text string) error
	// Users lists every user's name, sorted.
	Users(ctx context.Context) ([]string, error)
}

var _ API = (*Client)(nil)
