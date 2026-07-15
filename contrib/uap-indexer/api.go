package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

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

func newAPIServer(idx *Index, rl *RateLimiter, trustProxy bool) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, idx.StatusSnapshot())
	})

	// GET /positions?pubkey=<hex>[&unspent=true]
	mux.HandleFunc("/positions", func(w http.ResponseWriter, r *http.Request) {
		pubkey := r.URL.Query().Get("pubkey")
		if pubkey == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing pubkey query parameter"})
			return
		}
		unspentOnly := r.URL.Query().Get("unspent") == "true"
		positions := idx.PositionsForPubKey(pubkey, unspentOnly)
		if positions == nil {
			positions = []Position{}
		}
		writeJSON(w, http.StatusOK, positions)
	})

	// GET /position/{txid}/{vout}
	mux.HandleFunc("/position/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/position/"), "/")
		if len(parts) != 2 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected /position/{txid}/{vout}"})
			return
		}
		vout, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid vout"})
			return
		}
		pos, ok := idx.Position(parts[0], uint32(vout))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "position not found"})
			return
		}
		writeJSON(w, http.StatusOK, pos)
	})

	// POST /orders -- publish a signed maker order (see orders.go).
	// GET /orders?multiplier=1000 -- list open orders, optionally filtered.
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if !checkRateLimit(rl, trustProxy, w, r) {
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
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
			var multiplierFilter *int64
			if s := r.URL.Query().Get("multiplier"); s != "" {
				m, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multiplier"})
					return
				}
				multiplierFilter = &m
			}
			orders := idx.ListOrders(multiplierFilter)
			if orders == nil {
				orders = []Order{}
			}
			writeJSON(w, http.StatusOK, orders)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	// GET /orders/{txid}/{vout} -- fetch a single open order.
	// DELETE /orders/{txid}/{vout}?script_sig=<hex> -- withdraw it (see CancelOrder).
	mux.HandleFunc("/orders/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/orders/"), "/")
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
			o, ok := idx.GetOrder(parts[0], uint32(vout))
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "order not found"})
				return
			}
			writeJSON(w, http.StatusOK, o)

		case http.MethodDelete:
			if !checkRateLimit(rl, trustProxy, w, r) {
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
	})

	return mux
}
