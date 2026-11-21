// Package signin checks the one password for the web form and the app alike, and slows guessing down.
package signin

import (
	"crypto/sha256"
	"crypto/subtle"
	"sync"
	"time"
)

type Outcome int

const (
	Granted Outcome = iota
	Wrong
	// Throttled means too many wrong passwords came in lately; the password was not looked at.
	Throttled
)

const (
	clientBurst, clientEvery = 10, time.Minute
	allBurst, allEvery       = 100, 5 * time.Second
	maxClients               = 10000
)

// Throttle says whether wrong passwords are counted; a demo's password is public, so it has nothing to guard.
type Throttle bool

type Gate struct {
	want     [sha256.Size]byte
	throttle Throttle
	mu       sync.Mutex
	all      bucket
	clients  map[string]*bucket
}

func New(password string, throttle Throttle) *Gate {
	return &Gate{want: sha256.Sum256([]byte(password)), throttle: throttle, clients: map[string]*bucket{}}
}

// bucket counts recent wrong passwords; one is forgiven every so often.
type bucket struct {
	spent float64
	at    time.Time
}

func (b *bucket) settle(now time.Time, every time.Duration) {
	b.spent = max(0, b.spent-float64(now.Sub(b.at))/float64(every))
	b.at = now
}

// Check compares the password; client is the address guesses are counted against (intake.Client).
func (g *Gate) Check(client, password string) Outcome {
	got := sha256.Sum256([]byte(password))
	ok := subtle.ConstantTimeCompare(got[:], g.want[:]) == 1
	if bool(g.throttle) && !g.admit(client, ok) {
		return Throttled
	}
	if ok {
		return Granted
	}
	time.Sleep(time.Second)
	return Wrong
}

// admit refuses a client, or everyone, with no guesses left, and counts a wrong one.
func (g *Gate) admit(client string, ok bool) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.clients[client]
	if c == nil {
		g.prune(now)
		c = &bucket{at: now}
		g.clients[client] = c
	}
	c.settle(now, clientEvery)
	g.all.settle(now, allEvery)
	if c.spent+1 > clientBurst || g.all.spent+1 > allBurst {
		return false
	}
	if !ok {
		c.spent++
		g.all.spent++
	}
	return true
}

// prune keeps the table bounded: clients with nothing held against them go first, then everyone.
func (g *Gate) prune(now time.Time) {
	if len(g.clients) < maxClients {
		return
	}
	for k, b := range g.clients {
		if b.settle(now, clientEvery); b.spent == 0 {
			delete(g.clients, k)
		}
	}
	if len(g.clients) >= maxClients {
		g.clients = map[string]*bucket{}
	}
}
