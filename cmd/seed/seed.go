package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Jolls/Siftstr/internal/ids"
)

// File is the seed format: sources, and the summarized items that belong to
// them. It holds only content, never users, credentials or upstream IDs, so a
// file can be loaded into any instance.
type File struct {
	Sources []Source `json:"sources"`
	Items   []Item   `json:"items"`
}

// Source is one feed. Key links items to it within the file.
type Source struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Category    string `json:"category,omitempty"`
	Granularity string `json:"granularity,omitempty"` // default individual
	MaxDepth    string `json:"max_depth,omitempty"`   // default deep
	Carryover   string `json:"carryover,omitempty"`   // default carry
}

// Item is one summarized item. DaysAgo puts it on an earlier day, so it lands
// in the Backlog instead of Today.
type Item struct {
	Source       string `json:"source"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	MediaType    string `json:"media_type,omitempty"` // default article
	Excerpt      string `json:"excerpt,omitempty"`
	LightSummary string `json:"light_summary"`
	DeepSummary  string `json:"deep_summary,omitempty"` // makes the item `deep`
	DaysAgo      int    `json:"days_ago,omitempty"`
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// localNow is now in the user's timezone, as the app uses to pick the day.
func localNow(ctx context.Context, db *sql.DB, userID string, now time.Time) (time.Time, error) {
	var tz string
	if err := db.QueryRowContext(ctx, `SELECT timezone FROM users WHERE id = ?`, userID).Scan(&tz); err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return now.In(loc), nil
}

func userID(ctx context.Context, db *sql.DB, username string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no user %q: create one first (siftstr user add, or SIFTSTR_ADMIN_USER)", username)
	}
	return id, err
}

// load adds f's sources and items to username's data. Loading the same file
// again changes nothing. Seeded rows are marked with a "seed-" external ID.
func load(ctx context.Context, db *sql.DB, username string, f File, now time.Time) (sources, items int, err error) {
	uid, err := userID(ctx, db, username)
	if err != nil {
		return 0, 0, err
	}
	local, err := localNow(ctx, db, uid, now)
	if err != nil {
		return 0, 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	srcID := map[string]string{}
	for _, s := range f.Sources {
		if s.Key == "" || s.Name == "" {
			return 0, 0, fmt.Errorf("source needs a key and a name: %+v", s)
		}
		ext := "seed-" + s.Key
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM sources WHERE user_id = ? AND kind = 'miniflux_feed' AND external_id = ?`, uid, ext).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			if id, err = ids.New("src_"); err != nil {
				return 0, 0, err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO sources (id, user_id, kind, external_id, name, category, granularity, max_depth, carryover)
				 VALUES (?, ?, 'miniflux_feed', ?, ?, ?, ?, ?, ?)`,
				id, uid, ext, s.Name, s.Category, orDefault(s.Granularity, "individual"), orDefault(s.MaxDepth, "deep"), orDefault(s.Carryover, "carry")); err != nil {
				return 0, 0, fmt.Errorf("source %s: %w", s.Key, err)
			}
			sources++
		} else if err != nil {
			return 0, 0, err
		}
		srcID[s.Key] = id
	}

	stamp := now.UTC().Format(time.RFC3339)
	for _, it := range f.Items {
		sid, ok := srcID[it.Source]
		if !ok {
			return 0, 0, fmt.Errorf("item %q: unknown source %q", it.Title, it.Source)
		}
		state := "light"
		if it.DeepSummary != "" {
			state = "deep"
		}
		sum := sha1.Sum([]byte(it.Source + "\x00" + it.URL + "\x00" + it.Title))
		id, err := ids.New("itm_")
		if err != nil {
			return 0, 0, err
		}
		r, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO items (id, user_id, source_id, external_id, media_type, url, title, excerpt, state,
			   light_summary, deep_summary, batch_date, published_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?)`,
			id, uid, sid, "seed-"+hex.EncodeToString(sum[:6]), orDefault(it.MediaType, "article"), it.URL, it.Title, it.Excerpt, state,
			it.LightSummary, it.DeepSummary, local.AddDate(0, 0, -it.DaysAgo).Format("2006-01-02"),
			now.UTC().AddDate(0, 0, -it.DaysAgo).Format(time.RFC3339), stamp, stamp)
		if err != nil {
			return 0, 0, fmt.Errorf("item %q: %w", it.Title, err)
		}
		if n, _ := r.RowsAffected(); n > 0 {
			items++
		}
	}
	return sources, items, tx.Commit()
}

// export writes username's summarized items (light or deep) as a File. Source
// keys are s1, s2, ...; nothing else about the instance is included.
func export(ctx context.Context, db *sql.DB, username string, now time.Time) (File, error) {
	f := File{Sources: []Source{}, Items: []Item{}}
	uid, err := userID(ctx, db, username)
	if err != nil {
		return f, err
	}
	local, err := localNow(ctx, db, uid, now)
	if err != nil {
		return f, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT s.id, s.name, s.category, s.granularity, s.max_depth, s.carryover,
		        i.title, i.url, i.media_type, i.excerpt, COALESCE(i.light_summary, ''), COALESCE(i.deep_summary, ''), COALESCE(i.batch_date, '')
		 FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id
		 WHERE i.user_id = ? AND i.state IN ('light', 'deep') AND i.digest_id IS NULL
		 ORDER BY s.name, i.batch_date DESC, i.title`, uid)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	keys := map[string]string{}
	for rows.Next() {
		var sid, batch string
		var s Source
		var it Item
		if err := rows.Scan(&sid, &s.Name, &s.Category, &s.Granularity, &s.MaxDepth, &s.Carryover,
			&it.Title, &it.URL, &it.MediaType, &it.Excerpt, &it.LightSummary, &it.DeepSummary, &batch); err != nil {
			return f, err
		}
		key, ok := keys[sid]
		if !ok {
			key = fmt.Sprintf("s%d", len(keys)+1)
			keys[sid] = key
			s.Key = key
			f.Sources = append(f.Sources, s)
		}
		it.Source = key
		if d, err := time.ParseInLocation("2006-01-02", batch, local.Location()); err == nil {
			if days := int(local.Sub(d).Hours() / 24); days > 0 {
				it.DaysAgo = days
			}
		}
		f.Items = append(f.Items, it)
	}
	return f, rows.Err()
}
