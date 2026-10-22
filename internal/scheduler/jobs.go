package scheduler

import (
	"errors"
	"time"
)

// Job names. The server triggers jobs by name, so a typo would be a silent
// no-op; cmd/siftstr tests that every name here is registered.
const (
	JobIngestMiniflux = "ingest-miniflux"
	JobIngestNostr    = "ingest-nostr"
	JobKarakeep       = "karakeep"
	JobHighlights     = "highlights"
	JobOldSifting     = "old-sifting"
	JobReleaseActions = "release-actions"
	JobBriefing       = "briefing"
	JobOutbox         = "outbox"
	JobJanitor        = "janitor"
	JobBackup         = "backup"
)

// JobNames lists every job name above.
var JobNames = []string{JobIngestMiniflux, JobIngestNostr, JobKarakeep, JobHighlights, JobOldSifting, JobReleaseActions, JobBriefing, JobOutbox, JobJanitor, JobBackup}

// MinTriggerGap is the least time between triggered starts of one job for one
// user, counting timer runs. It protects upstreams (Nostr relays above all)
// from repeated saves. It is not configurable.
const MinTriggerGap = 30 * time.Second

// gapExempt jobs start a triggered run without waiting for MinTriggerGap.
var gapExempt = map[string]bool{JobOutbox: true, JobHighlights: true}

// Trigger errors.
var (
	ErrUnknownJob  = errors.New("scheduler: unknown job")
	ErrInstanceJob = errors.New("scheduler: an instance job cannot be triggered for a user")
	ErrNoUser      = errors.New("scheduler: a trigger needs a user ID")
)

// IngestJobForSourceKind returns the job that ingests sources of this kind
// (sources.kind: miniflux_feed, nostr, karakeep).
func IngestJobForSourceKind(kind string) (string, bool) {
	switch kind {
	case "miniflux_feed":
		return JobIngestMiniflux, true
	case "nostr":
		return JobIngestNostr, true
	case "karakeep":
		return JobKarakeep, true
	}
	return "", false
}
