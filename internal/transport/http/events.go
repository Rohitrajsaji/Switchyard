package httpapi

import (
	"net/http"
	"strings"
	"switchyard/internal/auth"
	"switchyard/internal/events"
	"time"
)

func (m *Management) ingestEvents(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		m.fail(w, r, auth.ErrUnauthorized)
		return
	}
	var in events.Batch
	if err := DecodeJSON(w, r, &in, 1<<20); err != nil {
		m.fail(w, r, err)
		return
	}
	receipts, err := events.New(m.pool, time.Now).Ingest(r.Context(), token, in)
	if err != nil {
		m.fail(w, r, err)
		return
	}
	if m.metrics != nil {
		for _, r := range receipts {
			m.metrics.ObserveEvent(r.Status, r.Duplicate)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, map[string]any{"receipts": receipts})
}
