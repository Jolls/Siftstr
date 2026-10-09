package triage

import (
	"context"
	"fmt"
)

// Which list a page shows.
const (
	ListToday   = "today"
	ListBacklog = "backlog"
)

// Card is one triage card as a page shows it: an item, or a digest standing
// in for its entries. Actioned cards stay in the list so they can show a
// badge and be undone (ARCHITECTURE.md section 3).
type Card struct {
	Kind       string // SubjectItem or SubjectDigest
	ID         string
	Title      string
	URL        string
	Source     string
	MediaType  string
	Summary    string
	State      string
	CanPromote bool
	// UndoID is the held action that put the card in its state, or empty when
	// nothing can be undone any more.
	UndoID string
}

// heldAction finds the newest action on a subject whose undo hold is open.
const heldAction = `(SELECT a.action_id FROM actions a
	WHERE a.user_id = %[1]s.user_id AND a.subject_type = '%[2]s' AND a.subject_id = %[1]s.id
	  AND a.status = 'held' AND a.kind != 'undo' AND a.release_at > ?
	ORDER BY a.client_at DESC, a.received_at DESC LIMIT 1)`

// visible is the states a list may show. Waiting and expired items are not
// shown anywhere.
const visible = `('light', 'deep', 'archived', 'kept', 'pending_deep')`

// Cards returns userID's cards for a list, given the user's local date.
// Today is everything summarized today. Backlog is what was carried over:
// still-untriaged cards from earlier days, plus earlier cards actioned
// recently enough to be undone.
func (s *Service) Cards(ctx context.Context, userID, list, today string) ([]Card, error) {
	now := stamp(s.Now().UTC())
	where := func(alias string) string {
		if list == ListToday {
			return alias + `.batch_date = ? AND ` + alias + `.state IN ` + visible
		}
		return alias + `.batch_date < ? AND (` + alias + `.state IN ('light', 'deep') OR held != '')`
	}

	itemQ := fmt.Sprintf(`SELECT i.id, i.title, i.url, s.name, i.media_type, i.state,
		COALESCE(NULLIF(i.deep_summary, ''), i.light_summary, ''), i.promoted_at IS NOT NULL, s.max_depth,
		COALESCE(`+heldAction+`, '') AS held
		FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id
		WHERE i.user_id = ? AND i.digest_id IS NULL AND `+where("i")+`
		ORDER BY COALESCE(i.published_at, i.created_at) DESC, i.id`, "i", SubjectItem)
	rows, err := s.DB.QueryContext(ctx, itemQ, now, userID, today)
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	var cards []Card
	for rows.Next() {
		c := Card{Kind: SubjectItem}
		var promoted bool
		var maxDepth string
		if err := rows.Scan(&c.ID, &c.Title, &c.URL, &c.Source, &c.MediaType, &c.State, &c.Summary, &promoted, &maxDepth, &c.UndoID); err != nil {
			rows.Close()
			return nil, err
		}
		// Promote asks for a deeper summary once, and only where the source
		// supports one. On a deep card it acts as keep.
		c.CanPromote = c.State == "deep" || (!promoted && maxDepth == "deep")
		cards = append(cards, c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	digestQ := fmt.Sprintf(`SELECT d.id, s.name, d.state, COALESCE(d.summary, ''),
		COALESCE(`+heldAction+`, '') AS held
		FROM digests d JOIN sources s ON s.user_id = d.user_id AND s.id = d.source_id
		WHERE d.user_id = ? AND `+where("d")+`
		ORDER BY d.batch_date DESC, d.id`, "d", SubjectDigest)
	drows, err := s.DB.QueryContext(ctx, digestQ, now, userID, today)
	if err != nil {
		return nil, fmt.Errorf("list digests: %w", err)
	}
	defer drows.Close()
	for drows.Next() {
		c := Card{Kind: SubjectDigest, MediaType: "digest"}
		if err := drows.Scan(&c.ID, &c.Source, &c.State, &c.Summary, &c.UndoID); err != nil {
			return nil, err
		}
		c.Title = c.Source + " digest"
		cards = append(cards, c)
	}
	return cards, drows.Err()
}
