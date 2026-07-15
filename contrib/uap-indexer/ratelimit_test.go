package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenBucketAllowsUpToCapacityThenBlocks(t *testing.T) {
	b := &tokenBucket{tokens: 3, capacity: 3, rate: 0, last: time.Now(), lastSeen: time.Now()}
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("expected request %d to be allowed (within burst capacity)", i)
		}
	}
	if b.Allow() {
		t.Fatal("expected the 4th request to be blocked once capacity is exhausted")
	}
}

func TestTokenBucketRefillsOverTime(t *testing.T) {
	b := &tokenBucket{tokens: 0, capacity: 5, rate: 100, last: time.Now(), lastSeen: time.Now()} // 100 tokens/sec
	if b.Allow() {
		t.Fatal("expected no tokens available immediately")
	}
	time.Sleep(20 * time.Millisecond) // ~2 tokens should have accrued
	if !b.Allow() {
		t.Fatal("expected a token to have refilled after waiting")
	}
}

func TestRateLimiterPerKeyIsolation(t *testing.T) {
	rl := &RateLimiter{buckets: make(map[string]*tokenBucket), rate: 0, capacity: 1}
	if !rl.Allow("alice") {
		t.Fatal("alice's first request should be allowed")
	}
	if rl.Allow("alice") {
		t.Fatal("alice's second request should be blocked (capacity 1, no refill)")
	}
	if !rl.Allow("bob") {
		t.Fatal("bob should have his own independent bucket and be allowed")
	}
}

func TestCheckRateLimitReturns429WhenExhausted(t *testing.T) {
	rl := &RateLimiter{buckets: make(map[string]*tokenBucket), rate: 0, capacity: 1}
	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req.RemoteAddr = "1.2.3.4:5555"

	w1 := httptest.NewRecorder()
	if !checkRateLimit(rl, false, w1, req) {
		t.Fatal("expected the first request to be allowed")
	}

	w2 := httptest.NewRecorder()
	if checkRateLimit(rl, false, w2, req) {
		t.Fatal("expected the second request to be rate-limited")
	}
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("expected HTTP 429, got %d", w2.Code)
	}
}

func TestCheckRateLimitNilDisablesLimiting(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	w := httptest.NewRecorder()
	for i := 0; i < 100; i++ {
		if !checkRateLimit(nil, false, w, req) {
			t.Fatal("expected a nil RateLimiter to never block requests")
		}
	}
}

func TestClientIPIgnoresForwardedForByDefault(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")

	if got := clientIP(req, false); got != "9.9.9.9" {
		t.Errorf("expected RemoteAddr to be used when trustProxy=false, got %q", got)
	}
	if got := clientIP(req, true); got != "1.1.1.1" {
		t.Errorf("expected X-Forwarded-For to be used when trustProxy=true, got %q", got)
	}
}

// End-to-end through the real mux: hammer POST /orders and confirm the
// relay itself starts returning 429s, not just the limiter in isolation.
func TestOrdersEndpointIsRateLimited(t *testing.T) {
	idx := NewIndex()
	rl := &RateLimiter{buckets: make(map[string]*tokenBucket), rate: 0, capacity: 2}
	server := newAPIServer(idx, rl, false)

	makeReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/orders", nil)
		req.RemoteAddr = "5.5.5.5:1"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		return w
	}

	// First two requests consume the burst capacity; they'll fail
	// PublishOrder validation (empty body) but must NOT be rate-limited.
	for i := 0; i < 2; i++ {
		w := makeReq()
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly rate-limited", i)
		}
	}
	// Third request should be blocked by the limiter before validation runs.
	w := makeReq()
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected HTTP 429 on the 3rd request, got %d", w.Code)
	}
}
