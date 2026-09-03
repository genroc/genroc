package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Failure throttling for the password endpoint.
//
// It is the only guessable secret reachable from outside: a `genroc_sk_*` and the HMAC key are
// both 256 bits of randomness, but a password is whatever someone chose. bcrypt slows a guess to
// tens of milliseconds, which is not a limit -- it is a constant factor an attacker parallelises
// away.
//
// Two keys, because they stop different attacks. Per-EMAIL stops one account being ground down;
// per-ADDRESS stops one attacker spraying many accounts, which per-email counting never sees.

const (
	// Ten rather than five, and five minutes rather than fifteen. A person typing a generated
	// 16-character password gets it wrong more than twice, and locking them out for a quarter of
	// an hour is a worse outcome than the guesses it prevents -- which bcrypt has already made
	// expensive. 10 per 5 minutes is 120 an hour: far too slow to search anything but a list of
	// the most common passwords, which is not what a rate limit is the defence against.
	maxEmailFailures = 10
	// Higher, because one address is legitimately many people behind NAT -- and because behind
	// a proxy this counts everyone at once (see clientIP).
	maxAddrFailures = 60
	// Both the memory and the penalty: failures older than this are forgotten, and a key over
	// its budget stays refused until they are.
	failureWindow = 5 * time.Minute
	// Above this many tracked keys, expired ones are swept. Without a bound an attacker sending
	// a fresh address each time turns the limiter into the memory leak.
	sweepAbove = 4096
)

type limiter struct {
	// Owned by the server rather than living at package level: it is per-process state with a
	// lifetime, not a lookup table. See the root CLAUDE.md.
	mu      sync.Mutex
	windows map[string]*failWindow
	now     func() time.Time
}

type failWindow struct {
	count   int
	resetAt time.Time
}

func newLimiter() *limiter {
	return &limiter{windows: map[string]*failWindow{}, now: time.Now}
}

// allow reports whether an attempt may proceed, and how long to wait if not.
func (l *limiter) allow(key string, max int) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windows[key]
	if w == nil || !l.now().Before(w.resetAt) {
		return true, 0
	}
	if w.count < max {
		return true, 0
	}
	return false, w.resetAt.Sub(l.now())
}

// fail records one failed attempt against a key.
func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.windows[key]
	if w == nil || !now.Before(w.resetAt) {
		w = &failWindow{resetAt: now.Add(failureWindow)}
		l.windows[key] = w
	}
	w.count++
	if len(l.windows) > sweepAbove {
		for k, v := range l.windows {
			if !now.Before(v.resetAt) {
				delete(l.windows, k)
			}
		}
	}
}

// succeed clears a key's history. A correct password is evidence the attempts before it were a
// person mistyping, and leaving the count standing would lock them out on their next visit.
func (l *limiter) succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.windows, key)
}

// clientIP is the address to count against, taken from the CONNECTION and never from a header.
//
// X-Forwarded-For is written by the client on a direct connection, so counting it would let an
// attacker reset their own budget on every request -- worse than not limiting, because it looks
// like a limit. Behind a proxy this collapses to one key for everyone, which throttles more than
// intended rather than less, and the per-email limit is unaffected either way.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
