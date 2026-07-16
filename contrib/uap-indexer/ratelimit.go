package main

import (
	"net"
	"net/http"
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

// clientIP extracts the connecting IP from a request, ignoring any
// X-Forwarded-For header by default (trusting it blindly would let any
// client bypass the limiter simply by setting the header themselves,
// unless the relay is actually deployed behind a proxy that overwrites
// it). Pass -trustproxy to honor X-Forwarded-For instead, only
// appropriate when the relay truly sits behind a trusted reverse proxy.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return xff
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
