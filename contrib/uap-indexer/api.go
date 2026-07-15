package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newAPIServer(idx *Index) http.Handler {
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

	return mux
}
