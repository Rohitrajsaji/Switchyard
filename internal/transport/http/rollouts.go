package httpapi

import (
	"net/http"

	"switchyard/internal/auth"
	"switchyard/internal/rollouts"
)

func (m *Management) rolloutService() *rollouts.Service { return rollouts.New(m.pool, nil) }

func (m *Management) createRollout(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in rollouts.CreateInput
	if err := DecodeJSON(w, r, &in, 65536); err != nil {
		m.fail(w, r, err)
		return
	}
	p, err := m.rolloutService().Create(r.Context(), actor, r.PathValue("project"), in, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 201, p)
}
func (m *Management) listRollouts(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	items, err := m.rolloutService().List(r.Context(), actor, r.PathValue("project"), r.URL.Query().Get("environment_id"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, items)
}
func (m *Management) getRollout(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	p, err := m.rolloutService().Get(r.Context(), actor, r.PathValue("project"), r.PathValue("rollout"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, p)
}
func (m *Management) rolloutChecks(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	items, err := m.rolloutService().Checks(r.Context(), actor, r.PathValue("project"), r.PathValue("rollout"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, items)
}
func (m *Management) approveRollout(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in struct {
		PlanHash string `json:"plan_hash"`
		Reason   string `json:"reason"`
	}
	if err := DecodeJSON(w, r, &in, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	p, err := m.rolloutService().Approve(r.Context(), actor, r.PathValue("project"), r.PathValue("rollout"), in.PlanHash, in.Reason, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, p)
}
func (m *Management) startRollout(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	p, err := m.rolloutService().Start(r.Context(), actor, r.PathValue("project"), r.PathValue("rollout"), w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, p)
}
func (m *Management) rejectRollout(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	m.stopRollout(w, r, actor, false)
}
func (m *Management) cancelRollout(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	m.stopRollout(w, r, actor, true)
}
func (m *Management) stopRollout(w http.ResponseWriter, r *http.Request, actor auth.Actor, cancel bool) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := DecodeJSON(w, r, &in, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	svc, project, id, request := m.rolloutService(), r.PathValue("project"), r.PathValue("rollout"), w.Header().Get("X-Request-ID")
	var p rollouts.Plan
	var err error
	if cancel {
		p, err = svc.Cancel(r.Context(), actor, project, id, in.Reason, request)
	} else {
		p, err = svc.Reject(r.Context(), actor, project, id, in.Reason, request)
	}
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, p)
}
func (m *Management) setExperimentTraffic(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in rollouts.TrafficInput
	if err := DecodeJSON(w, r, &in, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	d, err := m.rolloutService().SetTraffic(r.Context(), actor, r.PathValue("project"), r.PathValue("run"), in, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	m.invalidate(d.ProjectID, d.EnvironmentID, d.Key)
	JSON(w, 200, d)
}
