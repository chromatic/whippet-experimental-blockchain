package main

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var ErrNodeUnreachable = errors.New("node unreachable")

// FakeNode is a test implementation of NodeClient that records calls
// rather than actually sending transactions.
type FakeNode struct {
	Valid       map[string]bool // address -> validity
	Sent        []SentPayment   // recorded, never actually sent
	SendErr     error           // injectable failure
	ValidateErr error
}

type SentPayment struct {
	Address string
	Amount  float64
}

func (f *FakeNode) ValidateAddress(address string) (bool, error) {
	if f.ValidateErr != nil {
		return false, f.ValidateErr
	}
	if f.Valid == nil {
		return false, nil
	}
	return f.Valid[address], nil
}

func (f *FakeNode) SendToAddress(address string, amount float64) (string, error) {
	if f.SendErr != nil {
		return "", f.SendErr
	}
	f.Sent = append(f.Sent, SentPayment{Address: address, Amount: amount})
	return "mocktxid123", nil
}

// TestInvalidAddressNotSent verifies that an invalid address is rejected
// BEFORE any send is attempted. This is the most critical safety test.
func TestInvalidAddressNotSent(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	fake := &FakeNode{
		Valid: map[string]bool{
			"valid_address": true,
		},
	}

	handler := NewFaucetHandler(db, fake, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Submit invalid address
	resp, err := http.PostForm(srv.URL+"/", map[string][]string{
		"address": {"invalid_address"},
	})
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	// Verify request was rejected (400)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", resp.StatusCode)
	}

	// CRITICAL: Verify NO send was attempted
	if len(fake.Sent) != 0 {
		t.Errorf("Expected no sends for invalid address, but got %d", len(fake.Sent))
	}
}

// TestValidAddressExactlyOneSend verifies that a valid address produces
// exactly one send with the exact configured amount.
func TestValidAddressExactlyOneSend(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	const validAddr = "valid_address"
	const expectedAmount = 10.0

	fake := &FakeNode{
		Valid: map[string]bool{
			validAddr: true,
		},
	}

	handler := NewFaucetHandler(db, fake, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Submit valid address
	resp, err := http.PostForm(srv.URL+"/", map[string][]string{
		"address": {validAddr},
	})
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	// Verify success
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Verify exactly one send with correct address and amount
	if len(fake.Sent) != 1 {
		t.Errorf("Expected 1 send, got %d", len(fake.Sent))
	} else {
		if fake.Sent[0].Address != validAddr {
			t.Errorf("Expected address %q, got %q", validAddr, fake.Sent[0].Address)
		}
		if fake.Sent[0].Amount != expectedAmount {
			t.Errorf("Expected amount %.1f, got %.1f", expectedAmount, fake.Sent[0].Amount)
		}
	}
}

// TestSendErrorNotRecorded verifies that when SendErr is set, the HTTP
// response reports failure AND the database does not record a successful claim.
func TestSendErrorNotRecorded(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	const validAddr = "valid_address"

	fake := &FakeNode{
		Valid: map[string]bool{
			validAddr: true,
		},
		SendErr: ErrNodeUnreachable,
	}

	handler := NewFaucetHandler(db, fake, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Submit valid address
	resp, err := http.PostForm(srv.URL+"/", map[string][]string{
		"address": {validAddr},
	})
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	// Verify error response (should be 5xx)
	if resp.StatusCode < 500 || resp.StatusCode >= 600 {
		t.Errorf("Expected 5xx status, got %d", resp.StatusCode)
	}

	// Verify the claim was NOT recorded in database
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM requests WHERE address=?`, validAddr).Scan(&count)
	if err != nil {
		t.Fatalf("Database query failed: %v", err)
	}
	if count != 0 {
		t.Errorf("Expected 0 recorded claims, got %d (should not record failed sends)", count)
	}
}

// TestRateLimitBlocksWithoutSend verifies that the rate limit blocks a
// second request WITHOUT calling SendToAddress.
func TestRateLimitBlocksWithoutSend(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	const validAddr = "valid_address"

	fake := &FakeNode{
		Valid: map[string]bool{
			validAddr: true,
		},
	}

	handler := NewFaucetHandler(db, fake, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// First request succeeds
	resp1, err := http.PostForm(srv.URL+"/", map[string][]string{
		"address": {validAddr},
	})
	if err != nil {
		t.Fatalf("First POST failed: %v", err)
	}
	resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		t.Errorf("First request: expected status 200, got %d", resp1.StatusCode)
	}

	// Second request should be rate-limited
	resp2, err := http.PostForm(srv.URL+"/", map[string][]string{
		"address": {validAddr},
	})
	if err != nil {
		t.Fatalf("Second POST failed: %v", err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Errorf("Second request: expected status 429, got %d", resp2.StatusCode)
	}

	// Verify exactly ONE send (from first request only)
	if len(fake.Sent) != 1 {
		t.Errorf("Expected 1 send total, got %d (rate limit should block before send)", len(fake.Sent))
	}
}

// TestValidateErrorSurfacedAsServerError verifies that a ValidateErr
// (node unreachable) is surfaced as a server error, not as "address is invalid".
func TestValidateErrorSurfacedAsServerError(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	const validAddr = "valid_address"

	fake := &FakeNode{
		ValidateErr: ErrNodeUnreachable,
	}

	handler := NewFaucetHandler(db, fake, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.PostForm(srv.URL+"/", map[string][]string{
		"address": {validAddr},
	})
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	// Verify server error (5xx), not bad request (4xx)
	if resp.StatusCode < 500 || resp.StatusCode >= 600 {
		t.Errorf("ValidateErr should produce 5xx, got %d", resp.StatusCode)
	}
}

// setupTestDB creates a temporary test database with the faucet schema
func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	initDB(db)
	return db
}

// TestSameIPDifferentPorts tests that two requests from the same IP but different ports
// (which is the vulnerability: r.RemoteAddr includes port) are treated as the same client
func TestSameIPDifferentPorts(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// First request: IP:port1, no cookie
	ip := "192.168.1.1"
	ua := "Mozilla/5.0"
	cookie := ""

	// Simulate first request
	if canRequest(db, ip, ua, cookie) == false {
		t.Fatal("First request should be allowed")
	}
	logRequest(db, ip, ua, cookie, "address1")

	// Second request: same IP, same UA, but different (simulated) port
	// In real scenario, they'd have different ports, but we're testing with same IP
	// The fix ensures the IP alone (without port) blocks them
	if canRequest(db, ip, ua, cookie) == true {
		t.Fatal("Second request from same IP within 24 hours should be denied")
	}
}

// TestSameIPNoCookie tests that missing cookie doesn't create a unique per-request identity
// This is the main vulnerability: a missing cookie generates a unique nanosecond-based ID
func TestSameIPNoCookie(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ip := "192.168.1.2"
	ua := "curl/7.68.0"

	// First request with no cookie - should be allowed
	firstCookie := ""
	if canRequest(db, ip, ua, firstCookie) == false {
		t.Fatal("First request should be allowed")
	}
	logRequest(db, ip, ua, firstCookie, "address1")

	// Second request from same IP, still no cookie
	// With the vulnerability, each request gets a new unique ID, so this would pass
	// With the fix, empty cookie means we rely on IP check, so this should fail
	secondCookie := ""
	if canRequest(db, ip, ua, secondCookie) == true {
		t.Fatal("Second request from same IP within 24 hours (even without cookie) should be denied")
	}
}

// TestSameIPDifferentUA tests that different User-Agents from the same IP are still blocked
func TestSameIPDifferentUA(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ip := "192.168.1.3"
	cookie := ""

	// First request with UA1
	ua1 := "Mozilla/5.0"
	if canRequest(db, ip, ua1, cookie) == false {
		t.Fatal("First request should be allowed")
	}
	logRequest(db, ip, ua1, cookie, "address1")

	// Second request from same IP with different UA
	// The OR logic means: if any signal (ip, ua, or cookie) matches, deny
	// So the IP match alone should deny
	ua2 := "curl/7.68.0"
	if canRequest(db, ip, ua2, cookie) == true {
		t.Fatal("Second request from same IP within 24 hours should be denied, even with different UA")
	}
}

// TestDifferentIPs tests that requests from different IPs are both allowed
func TestDifferentIPs(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ua := "Mozilla/5.0"
	cookie := ""

	// Request from IP1
	ip1 := "192.168.1.4"
	if canRequest(db, ip1, ua, cookie) == false {
		t.Fatal("First request from IP1 should be allowed")
	}
	logRequest(db, ip1, ua, cookie, "address1")

	// Request from IP2
	ip2 := "192.168.1.5"
	if canRequest(db, ip2, ua, cookie) == false {
		t.Fatal("Request from different IP should be allowed")
	}
	logRequest(db, ip2, ua, cookie, "address2")

	// Verify first IP is still blocked
	if canRequest(db, ip1, ua, cookie) == true {
		t.Fatal("First IP should still be blocked after second IP request")
	}
}

// TestExpiryWindow tests that requests older than 24 hours are allowed
func TestExpiryWindow(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ip := "192.168.1.6"
	ua := "Mozilla/5.0"
	cookie := ""

	// Simulate old request (more than 24 hours ago)
	oldTimestamp := time.Now().Add(-25 * time.Hour).Unix()
	_, err := db.Exec(
		`INSERT INTO requests (ip, ua, cookie, address, timestamp) VALUES (?, ?, ?, ?, ?)`,
		ip, ua, cookie, "oldaddress", oldTimestamp,
	)
	if err != nil {
		t.Fatalf("Failed to log old request: %v", err)
	}

	// New request from same IP should be allowed (old request expired)
	if canRequest(db, ip, ua, cookie) == false {
		t.Fatal("Request should be allowed when previous request is older than 24 hours")
	}
}

// TestCookieValue tests that an existing cookie is used correctly
func TestCookieValue(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ip1 := "192.168.1.7"
	ua := "Mozilla/5.0"
	cookie := "user123"

	// First request with cookie
	if canRequest(db, ip1, ua, cookie) == false {
		t.Fatal("First request with cookie should be allowed")
	}
	logRequest(db, ip1, ua, cookie, "address1")

	// Second request from different IP but same cookie
	// The cookie should still block (OR logic: cookie=? matches)
	ip2 := "192.168.1.8"
	if canRequest(db, ip2, ua, cookie) == true {
		t.Fatal("Second request with same cookie should be denied, even from different IP")
	}
}

// TestClientIPTrustProxyFalseIgnoresHeader tests that when trustProxy=false,
// X-Forwarded-For header is completely ignored
func TestClientIPTrustProxyFalseIgnoresHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")

	result := clientIP(req, false)
	expected := "9.9.9.9"
	if result != expected {
		t.Errorf("clientIP(trustProxy=false) = %q, expected %q (should ignore X-Forwarded-For)", result, expected)
	}
}

// TestClientIPTrustProxyTrueUsesHeader tests that when trustProxy=true,
// X-Forwarded-For header is used instead of RemoteAddr
func TestClientIPTrustProxyTrueUsesHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "127.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")

	result := clientIP(req, true)
	expected := "1.1.1.1"
	if result != expected {
		t.Errorf("clientIP(trustProxy=true) = %q, expected %q (should use X-Forwarded-For)", result, expected)
	}
}

// TestClientIPTrustProxyTrueNoHeaderFallback tests that when trustProxy=true
// but no X-Forwarded-For header is present, it falls back to RemoteAddr
func TestClientIPTrustProxyTrueNoHeaderFallback(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	// No X-Forwarded-For header

	result := clientIP(req, true)
	expected := "9.9.9.9"
	if result != expected {
		t.Errorf("clientIP(trustProxy=true, no header) = %q, expected %q (should fall back to RemoteAddr)", result, expected)
	}
}

// TestClientIPStripsPortsAllModes tests that ports are stripped in all modes
func TestClientIPStripsPortsAllModes(t *testing.T) {
	tests := []struct {
		name          string
		trustProxy    bool
		remoteAddr    string
		xForwardedFor string
		expected      string
	}{
		{
			name:       "IPv4 with port, trustProxy=false",
			trustProxy: false,
			remoteAddr: "192.168.1.1:5678",
			expected:   "192.168.1.1",
		},
		{
			name:          "IPv4 with port, trustProxy=true (uses header)",
			trustProxy:    true,
			remoteAddr:    "127.0.0.1:9999",
			xForwardedFor: "192.168.1.1",
			expected:      "192.168.1.1",
		},
		{
			name:       "IPv6 with port, trustProxy=false",
			trustProxy: false,
			remoteAddr: "[2001:db8::1]:5678",
			expected:   "2001:db8::1",
		},
		{
			name:          "IPv6 with port, trustProxy=true (uses header)",
			trustProxy:    true,
			remoteAddr:    "[::1]:9999",
			xForwardedFor: "2001:db8::1",
			expected:      "2001:db8::1",
		},
		{
			name:       "No port, trustProxy=false",
			trustProxy: false,
			remoteAddr: "192.168.1.1",
			expected:   "192.168.1.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xForwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tt.xForwardedFor)
			}

			result := clientIP(req, tt.trustProxy)
			if result != tt.expected {
				t.Errorf("clientIP() = %q, expected %q", result, tt.expected)
			}
		})
	}
}

// TestClientIPCommaSeparatedList tests that comma-separated X-Forwarded-For lists
// extract the LAST entry (appended by the trusted proxy, not attacker-controlled).
// This is the fixed behavior: "attacker-ip, real-client-ip" -> extract "real-client-ip".
func TestClientIPCommaSeparatedList(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "127.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	// When trustProxy=true with comma-separated list, extract the LAST entry
	result := clientIP(req, true)
	expected := "5.6.7.8"
	if result != expected {
		t.Errorf("clientIP(trustProxy=true, comma-separated) = %q, expected %q (should extract last entry)", result, expected)
	}
}

// TestClientIPEmptyHeaderFallback tests that an empty X-Forwarded-For header
// (or trailing commas) falls back to RemoteAddr
func TestClientIPEmptyHeaderFallback(t *testing.T) {
	tests := []struct {
		name          string
		xForwardedFor string
		expected      string
	}{
		{"empty header", "", "9.9.9.9"},
		{"trailing comma", "1.1.1.1, ", "9.9.9.9"},
		{"only commas", ", , ", "9.9.9.9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = "9.9.9.9:1234"
			if tt.xForwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tt.xForwardedFor)
			}
			result := clientIP(req, true)
			if result != tt.expected {
				t.Errorf("clientIP() = %q, expected %q", result, tt.expected)
			}
		})
	}
}

// TestClientIPWhitespaceHandling tests that whitespace around commas is trimmed
func TestClientIPWhitespaceHandling(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "127.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "  1.1.1.1  ,  2.2.2.2  ")

	result := clientIP(req, true)
	expected := "2.2.2.2"
	if result != expected {
		t.Errorf("clientIP(whitespace) = %q, expected %q", result, expected)
	}
}

// TestClientIPSingleEntry tests that a single entry (no commas) is returned as-is
func TestClientIPSingleEntry(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "127.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")

	result := clientIP(req, true)
	expected := "203.0.113.7"
	if result != expected {
		t.Errorf("clientIP(single entry) = %q, expected %q", result, expected)
	}
}

// TestClientIPBareIPv6 tests that bare IPv6 addresses (without brackets) are handled
func TestClientIPBareIPv6(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "127.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "::1")

	result := clientIP(req, true)
	expected := "::1"
	if result != expected {
		t.Errorf("clientIP(bare IPv6) = %q, expected %q", result, expected)
	}
}

// TestClientIPIPv4WithPort tests that IPv4:port entries have the port stripped
func TestClientIPIPv4WithPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "127.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "203.0.113.7:1234, 203.0.113.8:5678")

	result := clientIP(req, true)
	expected := "203.0.113.8"
	if result != expected {
		t.Errorf("clientIP(IPv4:port) = %q, expected %q (should strip port)", result, expected)
	}
}

// TestClientIPBracketedIPv6WithPort tests that bracketed IPv6:port entries are handled
func TestClientIPBracketedIPv6WithPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "127.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "[2001:db8::1]:1234, [2001:db8::2]:5678")

	result := clientIP(req, true)
	expected := "2001:db8::2"
	if result != expected {
		t.Errorf("clientIP(IPv6:port) = %q, expected %q (should strip port)", result, expected)
	}
}

// TestTrustProxyRateLimitingSecurityRedGreen tests the bug fix: with trustProxy=true,
// different X-Forwarded-For values from same proxy should be rate-limited separately
func TestTrustProxyRateLimitingSecurityRedGreen(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Simulate two requests from the same reverse proxy (127.0.0.1)
	// but with different spoofed X-Forwarded-For values (only works with trustProxy=true)

	// First request: RemoteAddr=127.0.0.1, X-Forwarded-For=1.1.1.1
	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.RemoteAddr = "127.0.0.1:5678"
	req1.Header.Set("X-Forwarded-For", "1.1.1.1")

	ip1 := clientIP(req1, true) // Should be "1.1.1.1"
	ua := "Mozilla/5.0"
	cookie := ""

	if canRequest(db, ip1, ua, cookie) == false {
		t.Fatal("First request should be allowed")
	}
	logRequest(db, ip1, ua, cookie, "address1")

	// Second request: same RemoteAddr (same proxy), different X-Forwarded-For=2.2.2.2
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.RemoteAddr = "127.0.0.1:5678"
	req2.Header.Set("X-Forwarded-For", "2.2.2.2")

	ip2 := clientIP(req2, true) // Should be "2.2.2.2"

	// Since ip2 != ip1 and no cookie, second request should be allowed
	// This is correct behavior with trustProxy=true
	if canRequest(db, ip2, ua, cookie) == false {
		t.Fatal("Second request from different client IP (same proxy, different X-Forwarded-For) should be allowed")
	}
}

// TestTrustProxyDisabledHeaderNotSpoofable tests that when trustProxy=false,
// X-Forwarded-For spoofing has zero effect (the bug scenario)
func TestTrustProxyDisabledHeaderNotSpoofable(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// First request: RemoteAddr=9.9.9.9, with spoofed X-Forwarded-For (should be ignored)
	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.RemoteAddr = "9.9.9.9:1234"
	req1.Header.Set("X-Forwarded-For", "1.1.1.1")

	ip1 := clientIP(req1, false) // Should be "9.9.9.9" (ignores header)
	ua := "Mozilla/5.0"
	cookie := ""

	if canRequest(db, ip1, ua, cookie) == false {
		t.Fatal("First request should be allowed")
	}
	logRequest(db, ip1, ua, cookie, "address1")

	// Second request: same RemoteAddr, different spoofed X-Forwarded-For
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.RemoteAddr = "9.9.9.9:1234"
	req2.Header.Set("X-Forwarded-For", "2.2.2.2") // Attempt to spoof with different header

	ip2 := clientIP(req2, false) // Should still be "9.9.9.9" (ignores header)

	// Since ip2 == ip1 (both from 9.9.9.9), second request should be denied
	// This proves spoofing is blocked when trustProxy=false
	if canRequest(db, ip2, ua, cookie) == true {
		t.Fatal("Second request should be denied (same IP, header spoofing has no effect)")
	}
}

// TestTrustProxySpoofVulnerability (RED TEST) demonstrates the X-Forwarded-For spoofing
// vulnerability that exists in the current implementation. When trustProxy=true, the
// attacker sends: "X-Forwarded-For: <attacker-IP>, <real-client-IP>". The current
// implementation takes the leftmost (attacker-controlled) entry, allowing rate-limit bypass.
// This test confirms that both attacker IPs lead to rate-limit bypass with the vulnerable code.
// After the fix, both requests should resolve to the same real client IP (203.0.113.7).
func TestTrustProxySpoofVulnerability(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Simulate reverse proxy at 127.0.0.1 (RemoteAddr)
	// Real client is at 203.0.113.7 (appended by the trusted proxy)
	// Attacker tries to spoof by including their own IP in the header

	ua := "Mozilla/5.0"
	cookie := ""

	// First attack: attacker sends X-Forwarded-For = "1.1.1.1, 203.0.113.7"
	// With the fix, this should be parsed as: real client = 203.0.113.7 (rightmost)
	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.RemoteAddr = "127.0.0.1:5678"
	req1.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.7")

	ip1 := clientIP(req1, true)
	if canRequest(db, ip1, ua, cookie) == false {
		t.Fatal("First request should be allowed")
	}
	logRequest(db, ip1, ua, cookie, "address1")

	// Second attack: attacker sends different spoofed IP (2.2.2.2)
	// X-Forwarded-For = "2.2.2.2, 203.0.113.7"
	// With the fix, this should ALSO be parsed as: real client = 203.0.113.7 (rightmost)
	// So the second request should be rate-limited as the same client
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.RemoteAddr = "127.0.0.1:5678"
	req2.Header.Set("X-Forwarded-For", "2.2.2.2, 203.0.113.7")

	ip2 := clientIP(req2, true)

	// Both requests should resolve to the same real client IP (203.0.113.7)
	if ip1 != ip2 {
		t.Fatalf("Both requests should resolve to same real client IP, got ip1=%q, ip2=%q", ip1, ip2)
	}

	// Second request from same real client should be rate-limited
	if canRequest(db, ip2, ua, cookie) == true {
		t.Fatal("Second request from same client IP should be rate-limited, even with different spoofed X-Forwarded-For")
	}
}

func renderPage(content string) string {
	return `<!DOCTYPE html>
	<html lang="en">
	<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>Whippetcoin Faucet</title>
	<style>
	body { background: #f7fafc; font-family: sans-serif; display: flex; justify-content: center; align-items: center; height: 100vh; }
	.faucet-box { background: #fff; padding: 2em 2.5em; border-radius: 12px; box-shadow: 0 2px 16px #0001; min-width: 320px; }
	h1 { color: #3b5998; margin-bottom: 0.5em; }
	label { display: block; margin-bottom: 0.5em; font-weight: bold; }
	input[type="text"] { width: 100%; padding: 0.5em; border: 1px solid #ccc; border-radius: 6px; margin-bottom: 1em; font-size: 1em; }
	button { background: #3b5998; color: #fff; border: none; padding: 0.7em 1.5em; border-radius: 6px; font-size: 1em; cursor: pointer; transition: background 0.2s; }
	button:hover { background: #29487d; }
	.footer { margin-top: 1.5em; color: #888; font-size: 0.9em; text-align: center; }
	.footer a { color: #3b5998; text-decoration: none; }
	.footer a:hover { text-decoration: underline; }
	</style>
	</head>
	<body>
	<div class="faucet-box">
	` + content + `
	<div class="footer">One request per 24 hours. Powered by <a href="https://github.com/chromatic/whippet-experimental-blockchain" target="_blank">Whippetcoin</a>.</div>
	</div>
	</body>
	</html>`
}
