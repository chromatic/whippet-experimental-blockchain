package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// NodeBroadcaster is the only node capability the HTTP layer is allowed to
// reach. Deliberately two methods: this interface is the security boundary
// between the public API and the node's RPC, and widening it widens that.
type NodeBroadcaster interface {
	SendRawTransaction(hex string) (txid string, err error)
	EstimateSmartFee(blocks int) (satPerKB int64, err error)
}

// RecommendedMinTxFee is the minimum transaction fee from src/policy/policy.h.
// This is COIN/100 = 1,000,000 satoshis, used as a floor to prevent wallets
// from underpaying and having their transactions rejected by the network.
const RecommendedMinTxFee = 1000000

// writeStoreError turns a failed database read into a 500 and reports
// whether it handled the request. A read that fails is not the same as a
// lookup that found nothing: answering 404 or an empty list would tell the
// caller the chain does not contain something it may well contain.
func writeStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	log.Printf("index read failed: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "index unavailable"})
	return true
}

// writeStatus answers /status, choosing the HTTP status code from whether
// the index is still tracking the chain.
//
// writeStoreError above only covers a failed *read* -- StatusSnapshot's own
// queries erroring out. It says nothing about a latched write error: the
// database keeps answering reads perfectly well after the write path has
// stopped, so without this check /status would return 200 with a
// perpetually stale tip forever, and an operator polling it would see a
// service that looks healthy hours after it silently stopped indexing.
//
// A latched error gets HTTP 503 rather than 200. Most things that watch
// this endpoint for alerting purposes -- curl -f, container orchestrator
// liveness probes, uptime checkers -- key off the status code, not the
// response body, so that is the signal worth spending on. This does cost
// existing clients that assume /status always answers 200: to keep that
// cost as small as possible, the JSON body is unchanged in shape (a
// superset of the healthy response, not a different one), and it carries
// its own non-omitempty "healthy" boolean so a client that only ever reads
// the body -- never the status code -- still has an unambiguous field to
// gate on.
func writeStatus(w http.ResponseWriter, status Status) {
	if !status.Healthy {
		writeJSON(w, http.StatusServiceUnavailable, status)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// checkRateLimit returns true if the request may proceed. If not, it has
// already written a 429 response and the caller must not do anything
// further. rl may be nil to disable rate limiting entirely.
func checkRateLimit(rl *RateLimiter, trustProxy bool, w http.ResponseWriter, r *http.Request) bool {
	if rl == nil {
		return true
	}
	if rl.Allow(clientIP(r, trustProxy)) {
		return true
	}
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded, slow down"})
	return false
}

func newAPIServer(idx *Index, writeRL *RateLimiter, readRL *RateLimiter, trustProxy bool, broadcaster NodeBroadcaster) http.Handler {
	mux := http.NewServeMux()

	// Handler for /status endpoint (both /status and /api/status)
	statusHandler := func(w http.ResponseWriter, r *http.Request) {
		if !checkRateLimit(readRL, trustProxy, w, r) {
			return
		}
		status, err := idx.StatusSnapshot()
		if writeStoreError(w, err) {
			return
		}
		writeStatus(w, status)
	}
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/api/status", statusHandler)

	// GET /positions?pubkey=<hex>[&unspent=true] and /api/positions
	positionsHandler := func(w http.ResponseWriter, r *http.Request) {
		if !checkRateLimit(readRL, trustProxy, w, r) {
			return
		}
		pubkey := r.URL.Query().Get("pubkey")
		if pubkey == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing pubkey query parameter"})
			return
		}
		unspentOnly := r.URL.Query().Get("unspent") == "true"
		positions, err := idx.PositionsForPubKey(pubkey, unspentOnly)
		if writeStoreError(w, err) {
			return
		}
		if positions == nil {
			positions = []Position{}
		}
		writeJSON(w, http.StatusOK, positions)
	}
	mux.HandleFunc("/positions", positionsHandler)
	mux.HandleFunc("/api/positions", positionsHandler)

	// GET /position/{txid}/{vout} and /api/position/{txid}/{vout}
	positionHandler := func(w http.ResponseWriter, r *http.Request) {
		if !checkRateLimit(readRL, trustProxy, w, r) {
			return
		}
		// Extract the path after /position/ or /api/position/
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/position/") {
			path = strings.TrimPrefix(path, "/api/position/")
		} else {
			path = strings.TrimPrefix(path, "/position/")
		}
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected /position/{txid}/{vout}"})
			return
		}
		vout, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid vout"})
			return
		}
		pos, ok, err := idx.Position(parts[0], uint32(vout))
		if writeStoreError(w, err) {
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "position not found"})
			return
		}
		writeJSON(w, http.StatusOK, pos)
	}
	mux.HandleFunc("/position/", positionHandler)
	mux.HandleFunc("/api/position/", positionHandler)

	// POST /orders -- publish a signed maker order (see orders.go).
	// GET /orders?multiplier=1000 -- list open orders, optionally filtered.
	ordersHandler := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if !checkRateLimit(writeRL, trustProxy, w, r) {
				return
			}
			// Enforce max request body size (8KB) to prevent unbounded memory allocation.
			// This is generous for a legitimate signed order but far below dangerous levels.
			// http.MaxBytesReader will cause ReadAll to return an error if the body exceeds the limit,
			// and we'll return HTTP 413 Payload Too Large, not 500.
			const maxBodySize = 8192
			r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				// http.MaxBytesReader sets the response status to 413 automatically.
				// If we get here with a different error, it's likely a read error.
				if err.Error() == "http: request body too large" {
					writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
				} else {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
				}
				return
			}
			var o Order
			if err := json.Unmarshal(body, &o); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
				return
			}
			if err := idx.PublishOrder(&o); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, o)

		case http.MethodGet:
			if !checkRateLimit(readRL, trustProxy, w, r) {
				return
			}
			var multiplierFilter *int64
			if s := r.URL.Query().Get("multiplier"); s != "" {
				m, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multiplier"})
					return
				}
				multiplierFilter = &m
			}
			orders, err := idx.ListOrders(multiplierFilter)
			if writeStoreError(w, err) {
				return
			}
			if orders == nil {
				orders = []Order{}
			}
			writeJSON(w, http.StatusOK, orders)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
	mux.HandleFunc("/orders", ordersHandler)
	mux.HandleFunc("/api/orders", ordersHandler)

	// GET /orders/{txid}/{vout} -- fetch a single open order.
	// DELETE /orders/{txid}/{vout}?script_sig=<hex> -- withdraw it (see CancelOrder).
	singleOrderHandler := func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/orders/") {
			path = strings.TrimPrefix(path, "/api/orders/")
		} else {
			path = strings.TrimPrefix(path, "/orders/")
		}
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected /orders/{txid}/{vout}"})
			return
		}
		vout, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid vout"})
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !checkRateLimit(readRL, trustProxy, w, r) {
				return
			}
			o, ok, err := idx.GetOrder(parts[0], uint32(vout))
			if writeStoreError(w, err) {
				return
			}
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "order not found"})
				return
			}
			writeJSON(w, http.StatusOK, o)

		case http.MethodDelete:
			if !checkRateLimit(writeRL, trustProxy, w, r) {
				return
			}
			scriptSig := r.URL.Query().Get("script_sig")
			if scriptSig == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing script_sig query parameter"})
				return
			}
			if err := idx.CancelOrder(parts[0], uint32(vout), scriptSig); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
	mux.HandleFunc("/orders/", singleOrderHandler)
	mux.HandleFunc("/api/orders/", singleOrderHandler)

	// GET /utxos?address=<addr> or ?hash160=<hex> -- list UTXOs for an address or hash160
	utxosHandler := func(w http.ResponseWriter, r *http.Request) {
		if !checkRateLimit(readRL, trustProxy, w, r) {
			return
		}

		// Try to get hash160 from address parameter
		var hash160Hex string
		if addr := r.URL.Query().Get("address"); addr != "" {
			version, hash160, err := DecodeAddress(addr)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid address: " + err.Error()})
				return
			}
			// Verify version byte is one we recognize
			if version != MainnetVersion && version != TestnetVersion && version != RegtestVersion {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid address version byte"})
				return
			}
			hash160Hex = fmt.Sprintf("%x", hash160)
		} else if hashParam := r.URL.Query().Get("hash160"); hashParam != "" {
			// Validate it's valid hex
			if len(hashParam) != 40 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hash160 must be 40 hex characters"})
				return
			}
			hash160Hex = hashParam
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing address or hash160 query parameter"})
			return
		}

		// Get UTXOs for this hash160
		utxos, err := idx.UTXOsForHash160(hash160Hex)
		if writeStoreError(w, err) {
			return
		}
		if utxos == nil {
			utxos = []UTXO{}
		}
		writeJSON(w, http.StatusOK, utxos)
	}
	mux.HandleFunc("/utxos", utxosHandler)
	mux.HandleFunc("/api/utxos", utxosHandler)

	// GET /tokens -- list all lineages with aggregated stats
	tokensHandler := func(w http.ResponseWriter, r *http.Request) {
		if !checkRateLimit(readRL, trustProxy, w, r) {
			return
		}
		tokens, err := idx.AllTokens()
		if writeStoreError(w, err) {
			return
		}
		if tokens == nil {
			tokens = []TokenInfo{}
		}
		writeJSON(w, http.StatusOK, tokens)
	}
	mux.HandleFunc("/tokens", tokensHandler)
	mux.HandleFunc("/api/tokens", tokensHandler)

	// GET /token/{origin} -- fetch a single lineage by its mint origin
	tokenHandler := func(w http.ResponseWriter, r *http.Request) {
		if !checkRateLimit(readRL, trustProxy, w, r) {
			return
		}
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/token/") {
			path = strings.TrimPrefix(path, "/api/token/")
		} else {
			path = strings.TrimPrefix(path, "/token/")
		}
		origin := path
		if origin == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing origin"})
			return
		}
		token, ok, err := idx.Token(origin)
		if writeStoreError(w, err) {
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "token not found"})
			return
		}
		writeJSON(w, http.StatusOK, token)
	}
	mux.HandleFunc("/token/", tokenHandler)
	mux.HandleFunc("/api/token/", tokenHandler)

	// POST /broadcast -- broadcast a raw transaction to the network
	broadcastHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if !checkRateLimit(writeRL, trustProxy, w, r) {
			return
		}

		if broadcaster == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "broadcaster not available"})
			return
		}

		// Enforce max request body size (200KB)
		const maxBodySize = 200 * 1024
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if err.Error() == "http: request body too large" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body too large"})
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
			}
			return
		}

		var req struct {
			Hex string `json:"hex"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}

		if req.Hex == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing hex field"})
			return
		}

		// Validate hex string
		if len(req.Hex)%2 != 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hex string has odd length"})
			return
		}

		decoded, err := hex.DecodeString(req.Hex)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid hex encoding: " + err.Error()})
			return
		}

		// Validate transaction size (60 bytes to 100 KB)
		if len(decoded) < 60 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "transaction too small (minimum 60 bytes)"})
			return
		}
		if len(decoded) > 100*1024 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "transaction too large (maximum 100 KB)"})
			return
		}

		// Send to node
		txid, err := broadcaster.SendRawTransaction(req.Hex)
		if err != nil {
			// A rejection means whippetd saw the transaction and refused it:
			// that is the user's problem to fix, and its message is the only
			// useful thing we can show them. Anything else means the call
			// never reached the node, which is the operator's problem and
			// must not be reported as "your transaction is invalid".
			var rejection *NodeRejection
			if errors.As(err, &rejection) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": rejection.Message})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "node unreachable: " + err.Error()})
			return
		}

		// Validate the returned txid
		if !isTxid(txid) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "node returned invalid txid: " + txid})
			return
		}

		// Return the txid as a JSON string
		writeJSON(w, http.StatusOK, txid)
	}
	mux.HandleFunc("/broadcast", broadcastHandler)
	mux.HandleFunc("/api/broadcast", broadcastHandler)

	// GET /feerate -- estimate current fee rate
	feerateHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if !checkRateLimit(readRL, trustProxy, w, r) {
			return
		}

		if broadcaster == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "broadcaster not available"})
			return
		}

		// Call node to estimate fee for 6 blocks (Whippet's block time)
		satPerKB, err := broadcaster.EstimateSmartFee(6)

		var source string
		// If node fails or returns <= 0, use the policy floor
		if err != nil || satPerKB <= 0 {
			satPerKB = RecommendedMinTxFee
			source = "floor"
		} else {
			// Clamp upward to floor if needed
			if satPerKB < RecommendedMinTxFee {
				satPerKB = RecommendedMinTxFee
				source = "floor"
			} else {
				source = "node"
			}
		}

		resp := map[string]interface{}{
			"sat_per_kb": satPerKB,
			"source":     source,
		}
		writeJSON(w, http.StatusOK, resp)
	}
	mux.HandleFunc("/feerate", feerateHandler)
	mux.HandleFunc("/api/feerate", feerateHandler)

	// Wrap everything with security headers middleware
	apiWithHeaders := securityHeadersMiddleware(mux)
	webWithHeaders := securityHeadersMiddleware(http.HandlerFunc(serveWeb()))

	// Create a composite handler that routes to API or web based on the path
	finalMux := http.NewServeMux()

	// Register all API paths (both prefixed and unprefixed) to go through the API handler
	finalMux.Handle("/api/", apiWithHeaders)
	finalMux.Handle("/status", apiWithHeaders)
	finalMux.Handle("/positions", apiWithHeaders)
	finalMux.Handle("/position/", apiWithHeaders)
	finalMux.Handle("/orders", apiWithHeaders)
	finalMux.Handle("/orders/", apiWithHeaders)
	finalMux.Handle("/utxos", apiWithHeaders)
	finalMux.Handle("/tokens", apiWithHeaders)
	finalMux.Handle("/token/", apiWithHeaders)
	finalMux.Handle("/broadcast", apiWithHeaders)
	finalMux.Handle("/feerate", apiWithHeaders)

	// Register the web handler with security headers for root and unknown paths
	finalMux.Handle("/", webWithHeaders)

	return finalMux
}

// isTxid returns true if s is a valid txid (64 lowercase hex characters).
func isTxid(s string) bool {
	matched, _ := regexp.MatchString(`^[a-f0-9]{64}$`, s)
	return matched
}
