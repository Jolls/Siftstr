package server

import (
	"sync"
	"time"
)

// oneTimeSecrets hands a freshly created API key from the POST that made it
// to the GET that shows it. The key lives in memory only, is tied to one
// session, is returned once, and expires, so a page refresh cannot create
// another key and the secret is never stored.
type oneTimeSecrets struct {
	mu   sync.Mutex
	ttl  time.Duration
	now  func() time.Time
	byID map[string]secretEntry
}

type secretEntry struct {
	value   string
	expires time.Time
}

func newOneTimeSecrets(ttl time.Duration) *oneTimeSecrets {
	return &oneTimeSecrets{ttl: ttl, now: time.Now, byID: map[string]secretEntry{}}
}

// put stores value for the session and drops anything already expired.
func (o *oneTimeSecrets) put(session, value string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	for k, e := range o.byID {
		if now.After(e.expires) {
			delete(o.byID, k)
		}
	}
	o.byID[session] = secretEntry{value: value, expires: now.Add(o.ttl)}
}

// take returns and removes the session's value, if it is still fresh.
func (o *oneTimeSecrets) take(session string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, ok := o.byID[session]
	delete(o.byID, session)
	if !ok || o.now().After(e.expires) {
		return "", false
	}
	return e.value, true
}
