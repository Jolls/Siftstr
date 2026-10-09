package runs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MaxPromptLen bounds one stored prompt.
const MaxPromptLen = 20000

// Prompt names, also the suffix of the user_settings key "prompt_<name>".
const (
	PromptGlobal = "global"
	PromptLight  = "light"
	PromptDeep   = "deep"
	PromptDigest = "digest"
)

// PromptNames lists the editable prompts in display order.
var PromptNames = []string{PromptGlobal, PromptLight, PromptDeep, PromptDigest}

// Instructions are the prompts served inside a work package.
type Instructions struct {
	Global string `json:"global"`
	Light  string `json:"light"`
	Deep   string `json:"deep"`
	Digest string `json:"digest"`
}

// Defaults ship with the app. A user's saved prompt replaces the matching one.
var Defaults = Instructions{
	Global: `You are summarizing items for a daily triage queue. The reader will decide in a few seconds whether to archive an item, promote it for a deeper look, or keep it. Be accurate and neutral. Do not editorialize, do not pad, and never invent details that the source does not give. Write in Markdown. If an item has too little to go on, say so in one short line.`,
	Light:  `Write a light summary: one to three sentences, under 60 words, saying what the item is and why it might matter. Use only the title, excerpt and source name provided. Do not fetch the URL.`,
	Deep:   `Write a deeper summary of the item, 150 to 300 words. For an article, use the full content provided, or fetch the URL if the content is missing. For a video or podcast without a transcript, visit the page, skim the comments for reactions, and briefly look up the people or terms named in the title. End with one line on what a reader would gain from opening the original.`,
	Digest: `Write one digest for this source's entries today, as a short list of the notable items followed by a two-sentence overview. Group related entries. Mention how many entries there were.`,
}

func promptKey(name string) string { return "prompt_" + name }

// field returns a pointer to the named prompt, or nil for an unknown name.
// It is the only place that maps a prompt name to its struct field.
func (i *Instructions) field(name string) *string {
	switch name {
	case PromptGlobal:
		return &i.Global
	case PromptLight:
		return &i.Light
	case PromptDeep:
		return &i.Deep
	case PromptDigest:
		return &i.Digest
	}
	return nil
}

// Get returns the named prompt, or "" for an unknown name.
func (i Instructions) Get(name string) string {
	if p := i.field(name); p != nil {
		return *p
	}
	return ""
}

// Validation failures from SetPrompt.
var (
	ErrUnknownPrompt = errors.New("unknown prompt")
	ErrPromptTooLong = fmt.Errorf("the prompt is longer than %d characters", MaxPromptLen)
)

// Prompts returns userID's effective prompts: saved ones over the defaults.
func (s *Service) Prompts(ctx context.Context, userID string) (Instructions, error) {
	out := Defaults
	rows, err := s.DB.QueryContext(ctx,
		`SELECT key, value FROM user_settings WHERE user_id = ? AND key LIKE 'prompt\_%' ESCAPE '\'`, userID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return out, err
		}
		if p := out.field(strings.TrimPrefix(k, "prompt_")); p != nil {
			*p = v
		}
	}
	return out, rows.Err()
}

// SetPrompt saves one prompt. An empty (or all-space) value removes the saved
// one so the default applies again.
func (s *Service) SetPrompt(ctx context.Context, userID, name, value string) error {
	if !slices.Contains(PromptNames, name) {
		return ErrUnknownPrompt
	}
	value = strings.TrimSpace(value)
	if len(value) > MaxPromptLen {
		return ErrPromptTooLong
	}
	if value == Defaults.Get(name) {
		value = "" // saving the default means "use the default", so it keeps following updates
	}
	if value == "" {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM user_settings WHERE user_id = ? AND key = ?`, userID, promptKey(name))
		return err
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?)
		 ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value`, userID, promptKey(name), value)
	return err
}
