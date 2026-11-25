package web

import (
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

type Ceremony struct {
	Data    webauthn.SessionData
	UserID  string
	expires time.Time
}

type CeremonyStore struct {
	mu sync.Mutex
	M  map[string]Ceremony
}

func (c *CeremonyStore) Put(key string, cer Ceremony) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.M { // drop expired ones
		if now.After(v.expires) {
			delete(c.M, k)
		}
	}
	cer.expires = now.Add(5 * time.Minute)
	c.M[key] = cer
}

// take returns and removes a ceremony: each challenge can be answered once.
func (c *CeremonyStore) Take(key string) (Ceremony, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.M[key]
	delete(c.M, key)
	if !ok || time.Now().After(cer.expires) {
		return Ceremony{}, false
	}
	return cer, true
}
