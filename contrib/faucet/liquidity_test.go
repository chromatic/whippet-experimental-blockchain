package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// MockIndexer serves test token and order data over HTTP
type MockIndexer struct {
	tokens map[string]TokenInfo
	orders []Order
	status int // HTTP status to return
}

func (m *MockIndexer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(m.status)

	switch r.URL.Path {
	case "/api/tokens":
		tokens := make([]TokenInfo, 0, len(m.tokens))
		for _, t := range m.tokens {
			tokens = append(tokens, t)
		}
		json.NewEncoder(w).Encode(tokens)
	case "/api/orders":
		json.NewEncoder(w).Encode(m.orders)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// TestParRule_ExactlyAtPar: payment_value == backing_value should be allowed
func TestParRule_ExactlyAtPar(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	// Token with 1000 sat backing, lineage has 2+ holders
	// 20% of 1000 = 200, so an order of 100 is allowed
	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 1,
		Supply:     1000,
		Holders:    2,
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 100,
		BackingValue: 100,
		Origin:       "origin",
	}

	eligible, reason := engine.IsEligible(&order)
	if !eligible {
		t.Errorf("Order at par should be eligible, got reason: %s", reason)
	}
}

// TestParRule_OneSatoshiAbovePar: payment_value > backing_value should be rejected
func TestParRule_OneSatoshiAbovePar(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 1,
		Supply:     100,
		Holders:    2,
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 101,
		BackingValue: 100,
	}

	eligible, reason := engine.IsEligible(&order)
	if eligible {
		t.Errorf("Order above par should be rejected, got eligible: %v", eligible)
	}
	if !eligible && reason == "" {
		t.Error("Should provide a reason for rejection")
	}
}

// TestParRule_OneSatoshiBelowPar: payment_value < backing_value should be allowed
func TestParRule_OneSatoshiBelowPar(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 1,
		Supply:     1000,
		Holders:    2,
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 99,
		BackingValue: 100,
		Origin:       "origin",
	}

	eligible, reason := engine.IsEligible(&order)
	if !eligible {
		t.Errorf("Order below par should be eligible, got reason: %s", reason)
	}
}

// TestPurchaseCount_FirstThreeAllowed: first 3 purchases allowed per lineage
func TestPurchaseCount_FirstThreeAllowed(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     1000000,
		Holders:    2,
		MintHeight: 100,
	}

	// Record 3 prior purchases for this lineage
	for i := 0; i < 3; i++ {
		_, err := engine.db.Exec(
			`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
			origin,
			fmt.Sprintf("order%d:0", i),
			1000,
			time.Now().Unix(),
		)
		if err != nil {
			t.Fatalf("Failed to insert purchase: %v", err)
		}
	}

	order := Order{
		TxID:         "order4",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 1000,
		BackingValue: 1000,
		Origin:       origin,
	}

	eligible, reason := engine.IsEligible(&order)
	if !eligible {
		t.Errorf("4th purchase should be allowed (3 prior = 4 total), got reason: %s", reason)
	}
}

// TestPurchaseCount_FifthRejected: 5th purchase rejected
func TestPurchaseCount_FifthRejected(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     1000000,
		Holders:    2,
		MintHeight: 100,
	}

	// Record 4 prior purchases (the maximum)
	for i := 0; i < 4; i++ {
		_, err := engine.db.Exec(
			`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
			origin,
			fmt.Sprintf("order%d:0", i),
			1000,
			time.Now().Unix(),
		)
		if err != nil {
			t.Fatalf("Failed to insert purchase: %v", err)
		}
	}

	order := Order{
		TxID:         "order5",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 1000,
		BackingValue: 1000,
		Origin:       origin,
	}

	eligible, reason := engine.IsEligible(&order)
	if eligible {
		t.Errorf("5th purchase should be rejected, got eligible: %v", eligible)
	}
	if !eligible && reason == "" {
		t.Error("Should provide a reason for rejection")
	}
}

// TestSupplyCap_ExactlyAtTwentyPercent: 20% of supply allowed
func TestSupplyCap_ExactlyAtTwentyPercent(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	// Supply is 1000 sats, so 20% = 200 sats
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     1000,
		Holders:    2,
		MintHeight: 100,
	}

	// Already spent 190 sats of the 200 allowed
	_, err := engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		"order1:0",
		190,
		time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert purchase: %v", err)
	}

	// This purchase brings total to exactly 200 (20%)
	order := Order{
		TxID:         "order2",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 10,
		BackingValue: 10,
		Origin:       origin,
	}

	eligible, reason := engine.IsEligible(&order)
	if !eligible {
		t.Errorf("Purchase at exactly 20%% of supply should be eligible, got reason: %s", reason)
	}
}

// TestSupplyCap_OneSatoshiOver: one satoshi over 20% rejected
func TestSupplyCap_OneSatoshiOver(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	// Supply is 1000 sats, so 20% = 200 sats
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     1000,
		Holders:    2,
		MintHeight: 100,
	}

	// Already spent 200 sats (at the limit)
	_, err := engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		"order1:0",
		200,
		time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert purchase: %v", err)
	}

	// This purchase would put us 1 sat over the limit
	order := Order{
		TxID:         "order2",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 1,
		BackingValue: 1,
		Origin:       origin,
	}

	eligible, reason := engine.IsEligible(&order)
	if eligible {
		t.Errorf("Purchase over 20%% of supply should be rejected, got eligible: %v", eligible)
	}
	if !eligible && reason == "" {
		t.Error("Should provide a reason for rejection")
	}
}

// TestHoldersRule_OnlyOneHolder: token with 1 holder rejected
func TestHoldersRule_OnlyOneHolder(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 1,
		Supply:     1000, // 20% = 200, order of 50 is within limit, but holders check should fail
		Holders:    1,    // Not enough
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 50,
		BackingValue: 50,
		Origin:       "origin",
	}

	eligible, reason := engine.IsEligible(&order)
	if eligible {
		t.Errorf("Token with only 1 holder should be rejected, got eligible: %v", eligible)
	}
	if !eligible && reason == "" {
		t.Error("Should provide a reason for rejection")
	}
}

// TestHoldersRule_TwoHoldersAllowed: token with 2 holders allowed
func TestHoldersRule_TwoHoldersAllowed(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 1,
		Supply:     1000, // 20% = 200, so 50-sat order is within limit
		Holders:    2,    // Minimum required
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 50,
		BackingValue: 50,
		Origin:       "origin",
	}

	eligible, reason := engine.IsEligible(&order)
	if !eligible {
		t.Errorf("Token with 2 holders should be eligible, got reason: %s", reason)
	}
}

// TestDailyBudget_ExactlyAtCap: daily spending exactly at cap allowed
func TestDailyBudget_ExactlyAtCap(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     100000,
		Holders:    2,
		MintHeight: 100,
	}

	dailyLimit := int64(1000)
	engine.dailySpendCap = dailyLimit

	// Record purchase that brings us to exactly the limit
	_, err := engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		"order1:0",
		dailyLimit,
		time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert purchase: %v", err)
	}

	order := Order{
		TxID:         "order2",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 0, // Add nothing more to stay at cap
		BackingValue: 1, // Must be positive, but payment is zero so total spend stays same
		Origin:       origin,
	}

	eligible, _ := engine.IsEligible(&order)
	if !eligible {
		t.Errorf("Purchase at exactly daily cap should be allowed (no additional spend)")
	}
}

// TestDailyBudget_OneSatoshiOver: one satoshi over daily cap rejected
func TestDailyBudget_OneSatoshiOver(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     100000,
		Holders:    2,
		MintHeight: 100,
	}

	dailyLimit := int64(1000)
	engine.dailySpendCap = dailyLimit

	// Record purchase that brings us to exactly the limit
	_, err := engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		"order1:0",
		dailyLimit,
		time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert purchase: %v", err)
	}

	order := Order{
		TxID:         "order2",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 1,
		BackingValue: 1,
		Origin:       origin,
	}

	eligible, reason := engine.IsEligible(&order)
	if eligible {
		t.Errorf("Purchase over daily cap should be rejected, got eligible: %v", eligible)
	}
	if !eligible && reason == "" {
		t.Error("Should provide a reason for rejection")
	}
}

// TestDayRollover: yesterday's spending doesn't count against today
func TestDayRollover(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     100000,
		Holders:    2,
		MintHeight: 100,
	}

	dailyLimit := int64(1000)
	engine.dailySpendCap = dailyLimit

	// Record purchase from yesterday (>= 24 hours ago)
	yesterday := time.Now().Add(-25 * time.Hour).Unix()
	_, err := engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		"order1:0",
		dailyLimit,
		yesterday,
	)
	if err != nil {
		t.Fatalf("Failed to insert purchase: %v", err)
	}

	// Today, we should be able to spend the full daily cap again
	order := Order{
		TxID:         "order2",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 500,
		BackingValue: 500,
		Origin:       origin,
	}

	eligible, reason := engine.IsEligible(&order)
	if !eligible {
		t.Errorf("Yesterday's spending should not count today, got reason: %s", reason)
	}
}

// TestUnknownLineage: order for unknown lineage origin rejected
func TestUnknownLineage(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	// Don't add this origin to the indexer's tokens

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 50,
		BackingValue: 50,
	}

	eligible, reason := engine.IsEligible(&order)
	if eligible {
		t.Errorf("Order for unknown lineage should be rejected, got eligible: %v", eligible)
	}
	if !eligible && reason == "" {
		t.Error("Should provide a reason for rejection")
	}
}

// TestNegativeBackingValue: negative backing_value rejected
func TestNegativeBackingValue(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 1,
		Supply:     100,
		Holders:    2,
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 50,
		BackingValue: -10, // Invalid
	}

	eligible, reason := engine.IsEligible(&order)
	if eligible {
		t.Errorf("Order with negative backing_value should be rejected, got eligible: %v", eligible)
	}
	if !eligible && reason == "" {
		t.Error("Should provide a reason for rejection")
	}
}

// TestZeroBackingValue: zero backing_value rejected
func TestZeroBackingValue(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 1,
		Supply:     100,
		Holders:    2,
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 50,
		BackingValue: 0, // Invalid
	}

	eligible, reason := engine.IsEligible(&order)
	if eligible {
		t.Errorf("Order with zero backing_value should be rejected, got eligible: %v", eligible)
	}
	if !eligible && reason == "" {
		t.Error("Should provide a reason for rejection")
	}
}

// TestMultiplierSupplyConversion: ensure supply calculation is correct with multiplier
func TestMultiplierSupplyConversion(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	// If supply is 500 and multiplier is 2, total satoshis = 500 * 2 = 1000
	// 20% of that = 200 sats
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 2,
		Supply:     500,
		Holders:    2,
		MintHeight: 100,
	}

	// Already spent 200 sats (at the 20% limit)
	_, err := engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		"order1:0",
		200,
		time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert purchase: %v", err)
	}

	// One more sat would exceed 20%
	order := Order{
		TxID:         "order2",
		Vout:         0,
		Multiplier:   2,
		PaymentValue: 1,
		BackingValue: 1,
	}

	eligible, _ := engine.IsEligible(&order)
	if eligible {
		t.Errorf("Purchase exceeding 20%% of supply should be rejected, got eligible: %v", eligible)
	}
}

// TestExecutorExecute: executor records what it would buy without sending
func TestExecutorExecute(t *testing.T) {
	executor := &DryRunExecutor{}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		PaymentValue: 100,
	}

	err := executor.Execute(&order)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if len(executor.Executed) != 1 {
		t.Errorf("Expected 1 executed order, got %d", len(executor.Executed))
	}

	if executor.Executed[0].TxID != order.TxID {
		t.Errorf("Expected txid %s, got %s", order.TxID, executor.Executed[0].TxID)
	}

	if executor.TotalSpent != 100 {
		t.Errorf("Expected total spent 100, got %d", executor.TotalSpent)
	}
}

// TestDailyLedgerInsertion: purchase is recorded in the database
func TestDailyLedgerInsertion(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     100000,
		Holders:    2,
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 100,
		BackingValue: 100,
		Origin:       "origin",
	}

	err := engine.RecordPurchase(origin, &order)
	if err != nil {
		t.Fatalf("RecordPurchase failed: %v", err)
	}

	// Verify it's in the database
	var count int
	err = engine.db.QueryRow(
		`SELECT COUNT(*) FROM uap_purchases WHERE origin=? AND txid_vout=?`,
		origin,
		"order1:0",
	).Scan(&count)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	if count != 1 {
		t.Errorf("Expected 1 purchase in database, got %d", count)
	}
}

// Helper to create a test engine
type testLiquidityEngine struct {
	engine        *LiquidityEngine
	indexer       *MockIndexer
	db            *sql.DB
	dailySpendCap int64
	indexerServer *httptest.Server
}

func (t *testLiquidityEngine) IsEligible(order *Order) (bool, string) {
	// Sync tokens from mock indexer to engine
	t.engine.tokens = make(map[string]TokenInfo)
	for k, v := range t.indexer.tokens {
		t.engine.tokens[k] = v
	}
	t.engine.orders = t.indexer.orders
	// Sync dailySpendCap if it's been set on the test wrapper
	if t.dailySpendCap > 0 {
		t.engine.dailySpendCap = t.dailySpendCap
	}
	return t.engine.IsEligible(order)
}

func (t *testLiquidityEngine) RecordPurchase(origin string, order *Order) error {
	return t.engine.RecordPurchase(origin, order)
}

func makeTestLiquidityEngine(t *testing.T) *testLiquidityEngine {
	// Create in-memory database
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}

	// Initialize schema
	err = initLiquidityDB(db)
	if err != nil {
		t.Fatalf("Failed to init database: %v", err)
	}

	// Create mock indexer
	indexer := &MockIndexer{
		tokens: make(map[string]TokenInfo),
		orders: []Order{},
		status: http.StatusOK,
	}

	server := httptest.NewServer(indexer)

	// Create engine with httpClient
	engine := &LiquidityEngine{
		db:            db,
		indexerURL:    server.URL,
		dailySpendCap: 1000000, // Default to high limit
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		tokens: make(map[string]TokenInfo),
		orders: []Order{},
	}

	return &testLiquidityEngine{
		engine:        engine,
		indexer:       indexer,
		db:            db,
		dailySpendCap: 1000000,
		indexerServer: server,
	}
}

// TestZeroMultiplier: zero multiplier shouldn't cause panic but order may still be evaluated
func TestZeroMultiplier(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 0, // Invalid but shouldn't panic
		Supply:     100,
		Holders:    2,
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   0,
		PaymentValue: 50, // 50% of supply, exceeds 20% cap
		BackingValue: 50,
		Origin:       "origin",
	}

	engine.engine.tokens = map[string]TokenInfo{
		"origin": engine.indexer.tokens["origin"],
	}
	eligible, _ := engine.engine.IsEligible(&order)
	// Order exceeds 20% cap, so should be rejected
	if eligible {
		t.Errorf("Order exceeding 20%% cap should be rejected")
	}
}

// TestLargeMultiplierSupply: large multiplier values don't overflow
func TestLargeMultiplierSupply(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	// Large multiplier and supply - test that 20% calc doesn't overflow
	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 1000000, // Large multiplier
		Supply:     1000000, // Large supply
		Holders:    2,
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1000000,
		PaymentValue: 1000000, // 1000000 * 1000000 * 20% = 200000000000000
		BackingValue: 1000000,
		Origin:       "origin",
	}

	// Should not panic on overflow
	eligible, _ := engine.IsEligible(&order)
	// The order equals supply, which is way over 20%, so rejected
	if eligible {
		t.Errorf("Large order should be rejected for exceeding supply cap")
	}
}

// TestCumulativeSpendExactly20Percent: spending exactly 20% without prior purchases
func TestCumulativeSpendExactly20Percent(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     1000,
		Holders:    2,
		MintHeight: 100,
	}

	// First order: 20% of 1000 = 200
	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 200,
		BackingValue: 200,
		Origin:       origin,
	}

	eligible, reason := engine.IsEligible(&order)
	if !eligible {
		t.Errorf("Order for exactly 20%% of supply should be eligible, got reason: %s", reason)
	}
}

// TestCumulativeSpendMultipleOrders: spending just under and then over 20% cap
func TestCumulativeSpendMultipleOrders(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     1000,
		Holders:    2,
		MintHeight: 100,
	}

	// Insert 199 sats already spent (just under 20%)
	_, err := engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		"order0:0",
		199,
		time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert purchase: %v", err)
	}

	// Order for 1 sat brings us to exactly 200 (20%)
	order1 := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 1,
		BackingValue: 1,
		Origin:       origin,
	}

	engine.engine.tokens = map[string]TokenInfo{
		origin: engine.indexer.tokens[origin],
	}
	eligible, reason := engine.engine.IsEligible(&order1)
	if !eligible {
		t.Errorf("Order bringing total to 200 (20%%) should be allowed, got reason: %s", reason)
	}

	// Record order1 in the database
	_, err = engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		"order1:0",
		1,
		time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert order1: %v", err)
	}

	// Order for 1 more sat would exceed 20% (bringing total to 201)
	order2 := Order{
		TxID:         "order2",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 1,
		BackingValue: 1,
		Origin:       origin,
	}

	eligible, _ = engine.engine.IsEligible(&order2)
	if eligible {
		t.Errorf("Order exceeding 20%% cap should be rejected")
	}
}

// TestPurchaseCountBoundary: exactly 4 purchases allowed, 5th rejected
func TestPurchaseCountBoundary(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     10000000, // Large supply to avoid supply cap issues
		Holders:    2,
		MintHeight: 100,
	}

	// Check at each boundary
	for i := 0; i < 5; i++ {
		order := Order{
			TxID:         fmt.Sprintf("order%d", i),
			Vout:         0,
			Multiplier:   1,
			PaymentValue: 1,
			BackingValue: 1,
			Origin:       origin,
		}

		engine.engine.tokens = map[string]TokenInfo{
			origin: engine.indexer.tokens[origin],
		}
		eligible, reason := engine.engine.IsEligible(&order)

		if i < 4 {
			if !eligible {
				t.Errorf("Purchase %d should be allowed, got reason: %s", i+1, reason)
			}
			// Record it for next iteration
			engine.engine.RecordPurchase(origin, &order)
		} else {
			if eligible {
				t.Errorf("Purchase %d should be rejected (max 4 allowed)", i+1)
			}
		}
	}
}

// TestDailySpendAccumulation: daily spend accumulates correctly
func TestDailySpendAccumulation(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	origin := "minted:0"
	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: 1,
		Supply:     100000,
		Holders:    2,
		MintHeight: 100,
	}

	dailyLimit := int64(1000)
	engine.dailySpendCap = dailyLimit

	// Add 500 sats spent today
	_, err := engine.db.Exec(
		`INSERT INTO uap_purchases (origin, txid_vout, amount, timestamp) VALUES (?, ?, ?, ?)`,
		origin,
		"order1:0",
		500,
		time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert purchase: %v", err)
	}

	order1 := Order{
		TxID:         "order2",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 400, // Would total 900, still under limit
		BackingValue: 400,
		Origin:       origin,
	}

	engine.engine.dailySpendCap = dailyLimit
	engine.engine.tokens = map[string]TokenInfo{
		origin: engine.indexer.tokens[origin],
	}
	eligible, reason := engine.engine.IsEligible(&order1)
	if !eligible {
		t.Errorf("Order totaling 900 should be allowed under limit of 1000, got reason: %s", reason)
	}

	// Record it
	engine.engine.RecordPurchase(origin, &order1)

	// Try adding 200 more (would be 1100, over limit)
	order2 := Order{
		TxID:         "order3",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 200,
		BackingValue: 200,
		Origin:       origin,
	}

	eligible, _ = engine.engine.IsEligible(&order2)
	if eligible {
		t.Errorf("Order totaling 1100 should be rejected for exceeding daily limit of 1000")
	}
}

// TestBackingValueParCheckPrecision: par check is exact, not rounded
func TestBackingValueParCheckPrecision(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	engine.indexer.tokens["origin"] = TokenInfo{
		Origin:     "origin",
		Multiplier: 1,
		Supply:     10000,
		Holders:    2,
		MintHeight: 100,
	}

	// Payment exactly 1 satoshi more than backing
	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 1000001,
		BackingValue: 1000000,
		Origin:       "origin",
	}

	engine.engine.tokens = map[string]TokenInfo{
		"origin": engine.indexer.tokens["origin"],
	}
	eligible, _ := engine.engine.IsEligible(&order)
	if eligible {
		t.Errorf("Order with payment > backing by 1 sat should be rejected (par rule)")
	}
}

// TestHoldersCountAtBoundary: 1 holder rejected, 2 allowed
func TestHoldersCountAtBoundary(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	// Test with 1 holder
	engine.indexer.tokens["origin1"] = TokenInfo{
		Origin:     "origin1",
		Multiplier: 1,
		Supply:     10000,
		Holders:    1,
		MintHeight: 100,
	}

	order1 := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 100,
		BackingValue: 100,
		Origin:       "origin1",
	}

	engine.engine.tokens = map[string]TokenInfo{
		"origin1": engine.indexer.tokens["origin1"],
	}
	eligible, _ := engine.engine.IsEligible(&order1)
	if eligible {
		t.Errorf("Order for 1-holder token should be rejected")
	}

	// Test with 2 holders
	engine.indexer.tokens["origin2"] = TokenInfo{
		Origin:     "origin2",
		Multiplier: 1,
		Supply:     10000,
		Holders:    2,
		MintHeight: 100,
	}

	order2 := Order{
		TxID:         "order2",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 100,
		BackingValue: 100,
		Origin:       "origin2",
	}

	engine.engine.tokens["origin2"] = engine.indexer.tokens["origin2"]
	eligible, _ = engine.engine.IsEligible(&order2)
	if !eligible {
		t.Errorf("Order for 2-holder token should be allowed")
	}
}

// TestIndexerTokenLookup: order rejects if lineage not in indexer
func TestIndexerTokenLookup(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	// Don't add the origin to the indexer
	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   1,
		PaymentValue: 100,
		BackingValue: 100,
		Origin:       "unknown:0",
	}

	engine.engine.tokens = map[string]TokenInfo{} // Empty
	eligible, reason := engine.engine.IsEligible(&order)
	if eligible {
		t.Errorf("Order for unknown lineage should be rejected")
	}
	if reason == "" {
		t.Error("Should provide a reason")
	}
}

// TestSupplyCap_HonoursMultiplier is the test with teeth for the 20% rule.
// Every other supply test uses Multiplier: 1, where a token unit and a satoshi
// happen to coincide -- so they cannot tell the two apart, and stay green over
// a cap computed in the wrong denomination.
//
// TokenInfo.Supply is TOKEN UNITS: store.go computes it as
// satMul(unspentValue, multiplier). The satoshi backing is Supply/Multiplier.
// Spending is in satoshis, so the cap must be too; taking 20% of Supply
// directly inflates it by exactly the multiplier, and at any multiplier above
// 5 the concentration limit stops binding at all.
//
// Backing here is 1,000,000 sats at multiplier 1000, so Supply is
// 1,000,000,000 and the real cap is 200,000 sats. The order asks 200,001 --
// one satoshi over the true cap, and far under the mistaken 200,000,000 one.
func TestSupplyCap_HonoursMultiplier(t *testing.T) {
	engine := makeTestLiquidityEngine(t)
	defer engine.db.Close()

	const (
		origin     = "minted:0"
		multiplier = int64(1000)
		backing    = int64(1000000)
	)

	engine.indexer.tokens[origin] = TokenInfo{
		Origin:     origin,
		Multiplier: multiplier,
		Supply:     backing * multiplier,
		Holders:    2,
		MintHeight: 100,
	}

	order := Order{
		TxID:         "order1",
		Vout:         0,
		Multiplier:   multiplier,
		PaymentValue: 200001,
		BackingValue: backing,
		Origin:       origin,
	}

	eligible, reason := engine.IsEligible(&order)
	if eligible {
		t.Errorf("200001 sats exceeds 20%% of a 1000000 sat backing; expected rejection, got eligible (reason %q)", reason)
	}
}

// TestNewLiquidityEngine_BoundedByFaucetBudget pins the rule that X cannot be
// set above the budget the faucet already operates under. X is the one knob
// here that spends real money, and an operator who fat-fingers it has no
// second line of defence: the par rule stops the desk being drained, but says
// nothing about how much coin gets parked in inventory in a day.
//
// The bound is the drip's own implied daily budget -- faucet_amount coins per
// claim times daily_claim_limit claims -- because that is the number an
// operator has already chosen deliberately and already understands.
func TestNewLiquidityEngine_BoundedByFaucetBudget(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	initDB(db)
	if err := initLiquidityDB(db); err != nil {
		t.Fatalf("initLiquidityDB: %v", err)
	}

	// Defaults are 10.0 coins x 100 claims = 1000 coins.
	const impliedBudgetSats = int64(1000) * 100000000

	if _, err := NewLiquidityEngine(db, "http://127.0.0.1:1", impliedBudgetSats); err != nil {
		t.Errorf("X exactly at the implied budget should be accepted, got %v", err)
	}
	if _, err := NewLiquidityEngine(db, "http://127.0.0.1:1", impliedBudgetSats+1); err == nil {
		t.Error("X one satoshi above the implied budget should be refused")
	}
}
