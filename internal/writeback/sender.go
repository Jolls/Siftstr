package writeback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/dest"
	"github.com/Jolls/Siftstr/internal/outbox"
)

// Sender sends outbox entries through the destination for their connection.
type Sender struct {
	Conns     *connections.Service
	Factories map[string]dest.Factory // by connection kind
}

// Handle is the outbox.Runner handler. It never logs or returns a secret.
func (s *Sender) Handle(ctx context.Context, e outbox.Entry) error {
	if e.ConnectionID == "" {
		return dest.Permanent(errors.New("the connection was removed"))
	}
	conn, err := s.Conns.Get(ctx, e.UserID, e.ConnectionID)
	if errors.Is(err, connections.ErrNotFound) {
		return dest.Permanent(errors.New("the connection was removed"))
	} else if err != nil {
		return err
	}
	f, ok := s.Factories[conn.Kind]
	if !ok {
		return dest.Permanent(fmt.Errorf("no destination for %q", conn.Kind))
	}
	secret, err := s.Conns.Secret(ctx, e.UserID, e.ConnectionID)
	if err != nil && !errors.Is(err, connections.ErrNoSecret) {
		return err // unreadable: retry, the user may re-enter it
	}
	var item dest.Item
	if err := json.Unmarshal(e.Payload, &item); err != nil {
		return dest.Permanent(fmt.Errorf("bad payload: %w", err))
	}
	return f(conn.BaseURL, secret, conn.Config).Send(ctx, item)
}
