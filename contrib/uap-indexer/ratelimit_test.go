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

// TestClientIP is the whole contract of the -trustproxy flag in one table.
//
// The cases that matter most for security are the trustProxy=false ones: a
// client that sets X-Forwarded-For or X-Real-IP itself must not be able to
// change the key it is rate-limited under. Flipping the flag's default to
// true is exactly what those cases are there to catch.
func TestClientIP(t *testing.T) {
	const proxy = "127.0.0.1:5678" // what RemoteAddr looks like behind nginx
	const direct = "9.9.9.9:1234"  // what it looks like without a proxy

	tests := []struct {
		name       string
		trustProxy bool
		remoteAddr string
		xff        string // X-Forwarded-For, "" means not set at all
		xRealIP    string // X-Real-IP, same
		want       string
	}{
		// --- flag off: headers are ignored, full stop ---
		{
			name:       "off, spoofed XFF ignored",
			remoteAddr: direct,
			xff:        "1.1.1.1",
			want:       "9.9.9.9",
		},
		{
			name:       "off, spoofed X-Real-IP ignored",
			remoteAddr: direct,
			xRealIP:    "1.1.1.1",
			want:       "9.9.9.9",
		},
		{
			name:       "off, both headers spoofed, still ignored",
			remoteAddr: proxy,
			xff:        "1.1.1.1, 2.2.2.2",
			xRealIP:    "3.3.3.3",
			want:       "127.0.0.1",
		},
		{
			name:       "off, no headers",
			remoteAddr: direct,
			want:       "9.9.9.9",
		},

		// --- flag on: one trusted hop ---
		{
			name:       "on, single hop",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			// The core of the design: the caller supplied "1.1.1.1", nginx
			// appended the address it actually saw. Rightmost wins.
			name:       "on, attacker-supplied prefix is discarded",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "1.1.1.1, 203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "on, long attacker-supplied prefix is discarded",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "10.0.0.1, 172.16.0.1, evil, 203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "on, whitespace around entries",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "  1.1.1.1  ,  203.0.113.7  ",
			want:       "203.0.113.7",
		},

		// --- flag on: malformed input falls back, never fails ---
		{
			name:       "on, no headers at all",
			trustProxy: true,
			remoteAddr: direct,
			want:       "9.9.9.9",
		},
		{
			name:       "on, garbage header",
			trustProxy: true,
			remoteAddr: direct,
			xff:        "not-an-ip",
			want:       "9.9.9.9",
		},
		{
			// A rightmost entry that is garbage must NOT cause a scan
			// leftward for something parseable: everything left of it is
			// attacker-supplied.
			name:       "on, garbage rightmost entry does not fall back leftward",
			trustProxy: true,
			remoteAddr: direct,
			xff:        "1.1.1.1, garbage",
			want:       "9.9.9.9",
		},
		{
			// The proxy was supposed to append and did not, so there is no
			// proxy-written entry to trust.
			name:       "on, trailing comma",
			trustProxy: true,
			remoteAddr: direct,
			xff:        "1.1.1.1, ",
			want:       "9.9.9.9",
		},
		{
			name:       "on, only commas",
			trustProxy: true,
			remoteAddr: direct,
			xff:        ", , ",
			want:       "9.9.9.9",
		},
		{
			name:       "on, empty XFF falls through to X-Real-IP",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "",
			xRealIP:    "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "on, unusable XFF falls through to X-Real-IP",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "1.1.1.1, ",
			xRealIP:    "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "on, XFF wins over X-Real-IP when both usable",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "1.1.1.1, 203.0.113.7",
			xRealIP:    "198.51.100.4",
			want:       "203.0.113.7",
		},
		{
			name:       "on, garbage X-Real-IP falls back to RemoteAddr",
			trustProxy: true,
			remoteAddr: direct,
			xRealIP:    "still-not-an-ip",
			want:       "9.9.9.9",
		},

		// --- address shapes ---
		{
			name:       "on, bare IPv6",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "::1",
			want:       "::1",
		},
		{
			name:       "on, bare IPv6 with attacker prefix",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "1.1.1.1, 2001:db8::2",
			want:       "2001:db8::2",
		},
		{
			name:       "on, IPv4 entries carrying ports",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "203.0.113.7:1234, 203.0.113.8:5678",
			want:       "203.0.113.8",
		},
		{
			name:       "on, bracketed IPv6 entries carrying ports",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "[2001:db8::1]:1234, [2001:db8::2]:5678",
			want:       "2001:db8::2",
		},
		{
			name:       "on, bracketed IPv6 without a port",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "[2001:db8::2]",
			want:       "2001:db8::2",
		},
		{
			// Two spellings of one address must land in one bucket, or the
			// limiter can be evaded by varying the notation.
			name:       "on, IPv6 is canonicalized",
			trustProxy: true,
			remoteAddr: proxy,
			xff:        "2001:0DB8:0000:0000:0000:0000:0000:0002",
			want:       "2001:db8::2",
		},
		{
			name:       "off, IPv6 RemoteAddr",
			remoteAddr: "[2001:db8::5]:443",
			want:       "2001:db8::5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/orders", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xRealIP != "" {
				req.Header.Set("X-Real-IP", tt.xRealIP)
			}
			if got := clientIP(req, tt.trustProxy); got != tt.want {
				t.Errorf("clientIP(trustProxy=%v, RemoteAddr=%q, XFF=%q, X-Real-IP=%q) = %q, want %q",
					tt.trustProxy, tt.remoteAddr, tt.xff, tt.xRealIP, got, tt.want)
			}
		})
	}
}

// A client that varies a spoofed header must not get a fresh bucket each
// time. TestClientIP covers the parsing; this covers what the parsing is
// for, through the limiter itself, with the flag in both positions.
func TestSpoofedHeadersCannotEvadeRateLimit(t *testing.T) {
	for _, trustProxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "flag off", true: "flag on"}[trustProxy], func(t *testing.T) {
			rl := &RateLimiter{buckets: make(map[string]*tokenBucket), rate: 0, capacity: 1}
			spoof := func(prefix string) bool {
				req := httptest.NewRequest(http.MethodPost, "/orders", nil)
				req.RemoteAddr = "127.0.0.1:5678"
				req.Header.Set("X-Forwarded-For", prefix+", 203.0.113.7")
				return rl.Allow(clientIP(req, trustProxy))
			}
			if !spoof("1.1.1.1") {
				t.Fatal("first request should be allowed")
			}
			if spoof("2.2.2.2") {
				t.Fatal("second request should be rate-limited: changing the attacker-supplied " +
					"X-Forwarded-For prefix must not produce a new rate-limit bucket")
			}
		})
	}
}

// End-to-end through the real mux: hammer POST /orders and confirm the
// relay itself starts returning 429s, not just the limiter in isolation.
func TestOrdersEndpointIsRateLimited(t *testing.T) {
	idx := NewIndex()
	rl := &RateLimiter{buckets: make(map[string]*tokenBucket), rate: 0, capacity: 2}
	readRL := &RateLimiter{buckets: make(map[string]*tokenBucket), rate: 0, capacity: 100}
	server := newAPIServer(idx, rl, readRL, false, nil)

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
