package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenBucket is a classic lazily-refilled token bucket: tokens accrue at
// a fixed rate up to a capacity, and each allowed request consumes one.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	rate     float64 // tokens per second
	last     time.Time
	lastSeen time.Time
}

func (b *tokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.lastSeen = now

	b.tokens += elapsed * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// RateLimiter hands out a token bucket per key (typically client IP). It's
// a basic defense against a single client hammering the relay's write
// endpoints, not a substitute for real abuse protection (a reverse proxy
// or CDN) in front of a public deployment.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	rate     float64 // tokens per second
	capacity float64
}

// NewRateLimiter allows up to perMinute requests per minute per key,
// with burst extra requests permitted immediately before throttling
// kicks in. A background goroutine periodically evicts buckets that
// haven't been used in a while, so the map doesn't grow without bound
// under a wide spread of distinct client IPs.
func NewRateLimiter(perMinute float64, burst float64) *RateLimiter {
	rl := &RateLimiter{
		buckets:  make(map[string]*tokenBucket),
		rate:     perMinute / 60.0,
		capacity: burst,
	}
	go rl.evictStaleLoop()
	return rl
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: rl.capacity, capacity: rl.capacity, rate: rl.rate, last: time.Now(), lastSeen: time.Now()}
		rl.buckets[key] = b
	}
	rl.mu.Unlock()
	return b.Allow()
}

func (rl *RateLimiter) evictStaleLoop() {
	const idleTimeout = 10 * time.Minute
	ticker := time.NewTicker(idleTimeout)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-idleTimeout)
		rl.mu.Lock()
		for k, b := range rl.buckets {
			b.mu.Lock()
			stale := b.lastSeen.Before(cutoff)
			b.mu.Unlock()
			if stale {
				delete(rl.buckets, k)
			}
		}
		rl.mu.Unlock()
	}
}

// clientIP extracts the client's IP from a request, for use as a
// rate-limiting key (and as the identity any other abuse control would key
// on).
//
// It ignores proxy headers entirely unless trustProxy is set. That default
// is the security-relevant part: X-Forwarded-For and X-Real-IP are ordinary
// request headers that any client can set to anything, so honoring them
// unconditionally would let a caller pick a fresh identity per request and
// walk straight past the limiter. Only an operator who knows a reverse
// proxy is in front of this process -- and that the proxy overwrites those
// headers rather than passing the client's copy through -- can say the
// headers are trustworthy, so only the operator can turn this on
// (-trustproxy / UAP_TRUSTPROXY).
//
// When trustProxy is set, X-Forwarded-For is read RIGHTMOST-FIRST. The
// header is a comma-separated list that each hop APPENDS to (nginx's
// proxy_add_x_forwarded_for = "whatever the client sent, $remote_addr"), so
// a request arriving as
//
//	X-Forwarded-For: 1.1.1.1, 203.0.113.7
//
// means "someone claiming to be 1.1.1.1 connected to our proxy from
// 203.0.113.7". Everything left of the last entry was supplied by the
// caller and is worth exactly nothing; only the final entry was written by
// our own proxy. Taking the leftmost entry -- the common reading, because
// it is the "original client" in a chain of *trusted* proxies -- is the bug
// this flag exists to avoid: it hands the attacker the key.
//
// CRITICAL ASSUMPTION: exactly one trusted hop. With N chained trusted
// proxies the correct entry is the Nth from the right; this code always
// takes the 1st. Deploying behind a CDN plus your own nginx and leaving
// this as-is keys the limiter on the CDN's egress IP, not the user's.
//
// X-Real-IP (a single value, not a list -- nginx sets it to $remote_addr)
// is used only as a fallback when X-Forwarded-For is absent or unusable.
//
// Anything malformed falls back to RemoteAddr rather than failing the
// request: a rate limiter is not worth serving a 400 over, and RemoteAddr
// is always present and always genuine.
//
// This implementation must remain identical to contrib/faucet/faucet.go's
// clientIP for consistency; collapse into a shared helper when the services
// merge.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if ip, ok := forwardedForIP(r.Header.Get("X-Forwarded-For")); ok {
			return ip
		}
		if ip, ok := normalizeIP(r.Header.Get("X-Real-IP")); ok {
			return ip
		}
	}
	// Default, and the fallback for every malformed-header case above.
	if ip, ok := normalizeIP(r.RemoteAddr); ok {
		return ip
	}
	// RemoteAddr is not something a client controls and is essentially
	// always host:port, but if it is ever neither, use it verbatim rather
	// than collapsing every such request onto one shared "" bucket.
	return r.RemoteAddr
}

// forwardedForIP returns the rightmost usable entry of an X-Forwarded-For
// header. ok is false when the header is absent, empty, or contains nothing
// that parses as an IP address, in which case the caller falls back.
//
// Only the rightmost *valid* entry is considered: scanning further left for
// something parseable would walk into attacker-supplied territory, which is
// the whole thing this function exists to avoid. A trailing empty entry
// ("1.1.1.1, ") means the proxy did not append what it was supposed to, so
// the header cannot be trusted to have a proxy-written entry at all.
func forwardedForIP(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	entries := strings.Split(header, ",")
	return normalizeIP(entries[len(entries)-1])
}

// normalizeIP turns one address -- with or without a port, IPv4 or IPv6,
// bracketed or bare -- into a bare IP string. ok is false if what is left
// is not actually an IP address, so garbage never becomes a rate-limit key.
func normalizeIP(addr string) (string, bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", false
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	// Bare IPv6 ("::1", "2001:db8::1") reaches here unchanged: SplitHostPort
	// rejects it as "too many colons", which is the correct outcome -- there
	// is no port to strip.
	ip := net.ParseIP(strings.Trim(addr, "[]"))
	if ip == nil {
		return "", false
	}
	return ip.String(), true
}
