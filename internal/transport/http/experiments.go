package httpapi

import (
	"net/http"
	"switchyard/internal/auth"
	"switchyard/internal/experiments"
)

func (m *Management) listExperiments(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	runs, err := experiments.New(m.pool).List(r.Context(), actor, r.PathValue("project"), r.URL.Query().Get("environment_id"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, runs)
}
func (m *Management) getExperiment(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	run, err := experiments.New(m.pool).Get(r.Context(), actor, r.PathValue("project"), r.PathValue("run"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, run)
}
func (m *Management) createExperiment(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in experiments.CreateInput
	if err := DecodeJSON(w, r, &in, 65536); err != nil {
		m.fail(w, r, err)
		return
	}
	run, err := experiments.New(m.pool).Create(r.Context(), actor, r.PathValue("project"), in, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 201, run)
}
func (m *Management) transitionExperiment(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in experiments.TransitionInput
	if err := DecodeJSON(w, r, &in, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	run, err := experiments.New(m.pool).Transition(r.Context(), actor, r.PathValue("project"), r.PathValue("run"), in, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	m.invalidate(run.Definition.ProjectID, run.Definition.EnvironmentID, run.Definition.Key)
	JSON(w, 200, run)
}
