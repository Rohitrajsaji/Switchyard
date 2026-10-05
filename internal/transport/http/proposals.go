package httpapi

import (
	"net/http"

	"switchyard/internal/auth"
	"switchyard/internal/proposals"
)

func (m *Management) proposalService() *proposals.Service { return proposals.New(m.pool, nil) }

func (m *Management) createProposal(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in proposals.CreateInput
	if err := DecodeJSON(w, r, &in, 65536); err != nil {
		m.fail(w, r, err)
		return
	}
	p, err := m.proposalService().Create(r.Context(), actor, r.PathValue("project"), in, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 201, p)
}
func (m *Management) listProposals(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	items, err := m.proposalService().List(r.Context(), actor, r.PathValue("project"), r.URL.Query().Get("environment_id"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, items)
}
func (m *Management) getProposal(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	p, err := m.proposalService().Get(r.Context(), actor, r.PathValue("project"), r.PathValue("proposal"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, p)
}
func (m *Management) approveProposal(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in struct {
		DiffHash string `json:"diff_hash"`
		Reason   string `json:"reason"`
	}
	if err := DecodeJSON(w, r, &in, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	p, err := m.proposalService().Approve(r.Context(), actor, r.PathValue("project"), r.PathValue("proposal"), in.DiffHash, in.Reason, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, p)
}
func (m *Management) rejectProposal(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := DecodeJSON(w, r, &in, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	p, err := m.proposalService().Reject(r.Context(), actor, r.PathValue("project"), r.PathValue("proposal"), in.Reason, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, p)
}
func (m *Management) applyProposal(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	p, err := m.proposalService().Apply(r.Context(), actor, r.PathValue("project"), r.PathValue("proposal"), w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	m.invalidate(p.ProjectID, p.EnvironmentID, p.FlagKey)
	JSON(w, 200, p)
}
