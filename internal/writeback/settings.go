package writeback

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
)

// Settings reads and writes a user's keep destinations.
type Settings struct{ DB *sql.DB }

// Choice is where a user's kept videos and podcasts go, besides Miniflux.
type Choice struct {
	Video   []string
	Podcast []string
}

func (s *Settings) get(ctx context.Context, userID, key string, def []string) ([]string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM user_settings WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseKinds(v), nil
}

// Get returns the saved choice, or the defaults when none was saved.
func (s *Settings) Get(ctx context.Context, userID string) (Choice, error) {
	v, err := s.get(ctx, userID, SettingVideoDestinations, VideoCapable)
	if err != nil {
		return Choice{}, err
	}
	p, err := s.get(ctx, userID, SettingPodcastDestinations, []string{Karakeep})
	return Choice{Video: v, Podcast: p}, err
}

// Set saves the choice. Kinds that cannot take a video are dropped.
func (s *Settings) Set(ctx context.Context, userID string, c Choice) error {
	for key, kinds := range map[string][]string{SettingVideoDestinations: c.Video, SettingPodcastDestinations: c.Podcast} {
		kinds = slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return !slices.Contains(VideoCapable, k) })
		if _, err := s.DB.ExecContext(ctx,
			`INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?)
			 ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value`,
			userID, key, strings.Join(kinds, ",")); err != nil {
			return err
		}
	}
	return nil
}
