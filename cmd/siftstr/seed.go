package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Jolls/Siftstr/internal/config"
	"github.com/Jolls/Siftstr/internal/connections"
)

// seedConnections applies the connections from instance config to one user
// (ARCHITECTURE.md section 13). It logs the kind and username only.
func seedConnections(ctx context.Context, db *sql.DB, cfg config.Config, conns *connections.Service, logf func(string, ...any)) error {
	if len(cfg.Seeds) == 0 {
		return nil
	}
	userID, username, ok, err := seedUser(ctx, db, cfg.AdminUser)
	if err != nil {
		return fmt.Errorf("seed connections: %w", err)
	}
	if !ok {
		logf("connections from instance config not applied: no enabled user to receive them")
		return nil
	}
	for _, s := range cfg.Seeds {
		res, err := conns.ApplySeed(ctx, userID, connections.Seed{Kind: s.Kind, BaseURL: s.URL, Secret: s.APIKey})
		if err != nil {
			return fmt.Errorf("seed %s connection: %w", s.Kind, err)
		}
		switch {
		case res.Created:
			logf("created %s connection for %q from instance config", s.Kind, username)
		case res.URLUpdated && res.SecretSet:
			logf("updated %s connection URL and saved its key for %q from instance config", s.Kind, username)
		case res.URLUpdated:
			logf("updated %s connection URL for %q from instance config", s.Kind, username)
		case res.SecretSet:
			logf("saved %s connection key for %q from instance config", s.Kind, username)
		}
	}
	return nil
}

// seedUser picks the user that receives seeded connections: the one named by
// SIFTSTR_ADMIN_USER, else the oldest enabled admin.
func seedUser(ctx context.Context, db *sql.DB, adminUser string) (id, username string, ok bool, err error) {
	var row *sql.Row
	if name := strings.ToLower(strings.TrimSpace(adminUser)); name != "" {
		row = db.QueryRowContext(ctx, `SELECT id, username FROM users WHERE username = ? AND disabled = 0`, name)
	} else {
		row = db.QueryRowContext(ctx, `SELECT id, username FROM users WHERE role = 'admin' AND disabled = 0 ORDER BY created_at, id LIMIT 1`)
	}
	err = row.Scan(&id, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return id, username, true, nil
}
