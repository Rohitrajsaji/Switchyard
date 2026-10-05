package httpapi

import (
	"net/http"
	"switchyard/internal/auth"
	"switchyard/internal/metrics"
	"time"
)

func (m *Management) getResults(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	results, err := metrics.New(m.pool, time.Now).ReadAsync(r.Context(), actor, r.PathValue("project"), r.PathValue("run"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, results)
}
