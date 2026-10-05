package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/applicationeval"
	"switchyard/internal/audit"
	"switchyard/internal/auth"
	"switchyard/internal/cache"
	"switchyard/internal/experiments"
	"switchyard/internal/flags"
	"switchyard/internal/outbox"
	"switchyard/internal/projects"
	"switchyard/pkg/snapshot"
)

const SessionCookie = "switchyard_session"

type Management struct {
	auth         *auth.Service
	projects     *projects.Service
	pool         *pgxpool.Pool
	logger       *slog.Logger
	origin       string
	secure       bool
	loginLimit   *Limiter
	requestLimit *Limiter
	snapshots    *cache.Coordinator
	evaluator    *applicationeval.Service
}

func NewManagement(pool *pgxpool.Pool, logger *slog.Logger, origin string, secure bool, snapshots ...*cache.Coordinator) (*Management, error) {
	if len(snapshots) > 1 {
		return nil, errors.New("at most one evaluation cache may be configured")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("SWITCHYARD_ORIGIN must be an HTTP(S) origin")
	}
	a, err := auth.New(pool)
	if err != nil {
		return nil, err
	}
	m := &Management{auth: a, projects: projects.New(pool), pool: pool, logger: logger, origin: origin, secure: secure, loginLimit: NewLimiter(10, time.Minute, time.Now), requestLimit: NewLimiter(600, time.Minute, time.Now)}
	if len(snapshots) == 1 {
		m.snapshots = snapshots[0]
	}
	m.evaluator = applicationeval.New(pool, a, m.snapshots)
	return m, nil
}

func (m *Management) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/session", m.login)
	mux.HandleFunc("GET /v1/session", m.session)
	mux.HandleFunc("DELETE /v1/session", m.human(true, m.logout))
	mux.HandleFunc("GET /v1/projects", m.human(false, m.listProjects))
	mux.HandleFunc("POST /v1/projects", m.human(true, m.createProject))
	mux.HandleFunc("POST /v1/users", m.human(true, m.createUser))
	mux.HandleFunc("GET /v1/projects/{project}/environments", m.human(false, m.environments))
	mux.HandleFunc("POST /v1/projects/{project}/members", m.human(true, m.addMember))
	mux.HandleFunc("POST /v1/projects/{project}/application-keys", m.human(true, m.createKey))
	mux.HandleFunc("DELETE /v1/projects/{project}/application-keys/{key}", m.human(true, m.revokeKey))
	mux.HandleFunc("GET /v1/projects/{project}/audit", m.human(false, m.audit))
	mux.HandleFunc("GET /v1/projects/{project}/flags", m.human(false, m.listFlags))
	mux.HandleFunc("POST /v1/projects/{project}/flags", m.human(true, m.createFlag))
	mux.HandleFunc("GET /v1/projects/{project}/flags/{key}", m.human(false, m.getFlag))
	mux.HandleFunc("PUT /v1/projects/{project}/flags/{key}", m.human(true, m.updateFlag))
	mux.HandleFunc("POST /v1/projects/{project}/flags/{key}/preview", m.human(false, m.previewFlag))
	mux.HandleFunc("POST /v1/evaluate", m.evaluate)
	mux.HandleFunc("POST /v1/events", m.ingestEvents)
	mux.HandleFunc("GET /v1/projects/{project}/experiments", m.human(false, m.listExperiments))
	mux.HandleFunc("POST /v1/projects/{project}/experiments", m.human(true, m.createExperiment))
	mux.HandleFunc("GET /v1/projects/{project}/experiments/{run}", m.human(false, m.getExperiment))
	mux.HandleFunc("GET /v1/projects/{project}/experiments/{run}/results", m.human(false, m.getResults))
	mux.HandleFunc("POST /v1/projects/{project}/experiments/{run}/transitions", m.human(true, m.transitionExperiment))
}
func (m *Management) login(w http.ResponseWriter, r *http.Request) {
	if !m.limit(w, r, m.loginLimit) {
		return
	}
	if r.Header.Get("Origin") != m.origin {
		m.fail(w, r, auth.ErrForbidden)
		return
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := DecodeJSON(w, r, &input, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	if len(input.Email) > 254 || len(input.Password) > 72 {
		m.fail(w, r, auth.ErrUnauthorized)
		return
	}
	s, err := m.auth.Login(r.Context(), input.Email, input.Password)
	if err != nil {
		m.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: s.Token, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteStrictMode, Expires: s.ExpiresAt, MaxAge: 86400})
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, s)
}
func (m *Management) session(w http.ResponseWriter, r *http.Request) {
	if !m.limit(w, r, m.requestLimit) {
		return
	}
	cookie, err := r.Cookie(SessionCookie)
	if err != nil {
		m.fail(w, r, auth.ErrUnauthorized)
		return
	}
	s, err := m.auth.Current(r.Context(), cookie.Value)
	if err != nil {
		m.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, s)
}

type humanHandler func(http.ResponseWriter, *http.Request, auth.Actor)

func (m *Management) human(write bool, next humanHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !m.limit(w, r, m.requestLimit) {
			return
		}
		cookie, err := r.Cookie(SessionCookie)
		if err != nil {
			m.fail(w, r, auth.ErrUnauthorized)
			return
		}
		actor, err := m.auth.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			m.fail(w, r, err)
			return
		}
		if write {
			if r.Header.Get("Origin") != m.origin {
				m.fail(w, r, auth.ErrForbidden)
				return
			}
			if err := m.auth.VerifyCSRF(r.Context(), cookie.Value, r.Header.Get("X-CSRF-Token")); err != nil {
				m.fail(w, r, err)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r, actor)
	}
}
func (m *Management) logout(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil {
		m.fail(w, r, auth.ErrUnauthorized)
		return
	}
	if err := m.auth.Logout(r.Context(), cookie.Value); err != nil {
		m.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(204)
}
func (m *Management) listProjects(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	items, err := m.projects.List(r.Context(), actor)
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, items)
}
func (m *Management) createProject(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var input struct {
		Name string `json:"name"`
	}
	if err := DecodeJSON(w, r, &input, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	p, err := m.projects.Create(r.Context(), actor, input.Name, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 201, p)
}
func (m *Management) createUser(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := DecodeJSON(w, r, &input, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	u, err := m.projects.CreateUser(r.Context(), actor, input.Email, input.Password, input.Role, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 201, u)
}
func (m *Management) environments(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	result, err := m.projects.Environments(r.Context(), actor, r.PathValue("project"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, result)
}
func (m *Management) addMember(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var input struct {
		UserID string `json:"user_id"`
	}
	if err := DecodeJSON(w, r, &input, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	if err := m.projects.AddMember(r.Context(), actor, r.PathValue("project"), input.UserID, w.Header().Get("X-Request-ID")); err != nil {
		m.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (m *Management) createKey(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var input struct {
		Name          string   `json:"name"`
		EnvironmentID string   `json:"environment_id"`
		Permissions   []string `json:"permissions"`
	}
	if err := DecodeJSON(w, r, &input, 4096); err != nil {
		m.fail(w, r, err)
		return
	}
	k, err := m.projects.CreateKey(r.Context(), actor, r.PathValue("project"), input.EnvironmentID, input.Name, input.Permissions, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 201, k)
}
func (m *Management) revokeKey(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	if err := m.projects.RevokeKey(r.Context(), actor, r.PathValue("project"), r.PathValue("key"), w.Header().Get("X-Request-ID")); err != nil {
		m.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (m *Management) audit(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	projectID := r.PathValue("project")
	if err := auth.Authorize(r.Context(), m.pool, actor, projectID, "", false); err != nil {
		m.fail(w, r, err)
		return
	}
	var before int64
	if v := r.URL.Query().Get("before_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			m.fail(w, r, auth.ErrInvalid)
			return
		}
		before = n
	}
	items, err := audit.List(r.Context(), m.pool, projectID, before)
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, items)
}
func (m *Management) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := 500, "internal_error"
	switch {
	case errors.Is(err, outbox.ErrCapacity):
		status, code = 503, "durable_work_capacity"
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, auth.ErrUnauthorized):
		status, code = 401, "unauthorized"
	case errors.Is(err, auth.ErrForbidden):
		status, code = 403, "forbidden"
	case errors.Is(err, auth.ErrInvalid):
		status, code = 400, "invalid_input"
	case errors.Is(err, auth.ErrConflict):
		status, code = 409, "conflict"
	case errors.Is(err, flags.ErrNotFound), errors.Is(err, experiments.ErrNotFound):
		status, code = 404, "not_found"
	}
	if status == 500 {
		m.logger.Error("management operation failed", "route", r.Pattern, "request_id", w.Header().Get("X-Request-ID"))
	}
	JSON(w, status, map[string]string{"error": code, "request_id": w.Header().Get("X-Request-ID")})
}

func (m *Management) listFlags(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	items, err := flags.New(m.pool).List(r.Context(), actor, r.PathValue("project"), r.URL.Query().Get("environment_id"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, items)
}
func (m *Management) createFlag(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in flags.CreateInput
	if err := DecodeJSON(w, r, &in, 65536); err != nil {
		m.fail(w, r, err)
		return
	}
	d, err := flags.New(m.pool).Create(r.Context(), actor, r.PathValue("project"), in, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	m.invalidate(d.ProjectID, d.EnvironmentID, d.Key)
	JSON(w, 201, d)
}
func (m *Management) updateFlag(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in flags.UpdateInput
	if err := DecodeJSON(w, r, &in, 65536); err != nil {
		m.fail(w, r, err)
		return
	}
	d, err := flags.New(m.pool).Update(r.Context(), actor, r.PathValue("project"), r.PathValue("key"), in, w.Header().Get("X-Request-ID"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	m.invalidate(d.ProjectID, d.EnvironmentID, d.Key)
	JSON(w, 200, d)
}
func (m *Management) getFlag(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	project, env := r.PathValue("project"), r.URL.Query().Get("environment_id")
	if err := auth.Authorize(r.Context(), m.pool, actor, project, env, false); err != nil {
		m.fail(w, r, err)
		return
	}
	d, err := flags.New(m.pool).Get(r.Context(), project, env, r.PathValue("key"))
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, d)
}

type evaluationInput = applicationeval.Input
type evaluationResponse = applicationeval.Response

// EvaluationService is shared with the gRPC adapter in the API composition root.
func (m *Management) EvaluationService() *applicationeval.Service { return m.evaluator }

func (m *Management) evaluate(w http.ResponseWriter, r *http.Request) {
	var in evaluationInput
	if err := DecodeJSON(w, r, &in, 65536); err != nil {
		m.fail(w, r, err)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		m.fail(w, r, auth.ErrUnauthorized)
		return
	}
	response, err := m.evaluator.Evaluate(r.Context(), token, in)
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, cache.ErrUnavailable) {
		w.Header().Set("Retry-After", "1")
		JSON(w, 503, struct {
			evaluationResponse
			Error string `json:"error"`
		}{response, "configuration_unavailable"})
		return
	}
	if err != nil {
		m.fail(w, r, err)
		return
	}
	JSON(w, 200, response)
}
func (m *Management) invalidate(projectID, environmentID, key string) {
	if m.snapshots != nil {
		m.snapshots.Invalidate(snapshot.Key{ProjectID: projectID, EnvironmentID: environmentID, FlagKey: key})
	}
}
func (m *Management) previewFlag(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	var in evaluationInput
	if err := DecodeJSON(w, r, &in, 65536); err != nil {
		m.fail(w, r, err)
		return
	}
	in.ProjectID = r.PathValue("project")
	in.Key = r.PathValue("key")
	if err := auth.Authorize(r.Context(), m.pool, actor, in.ProjectID, in.EnvironmentID, false); err != nil {
		m.fail(w, r, err)
		return
	}
	m.evaluateInput(w, r, in)
}
func (m *Management) evaluateInput(w http.ResponseWriter, r *http.Request, in evaluationInput) {
	result, err := m.evaluator.Preview(r.Context(), in)
	if err != nil {
		m.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, result)
}
func DecodeJSON(w http.ResponseWriter, r *http.Request, target any, limit int64) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return auth.ErrInvalid
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return auth.ErrInvalid
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return auth.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return auth.ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return auth.ErrInvalid
	}
	return nil
}

type limitWindow struct {
	start time.Time
	count int
}
type Limiter struct {
	mu      sync.Mutex
	entries map[string]limitWindow
	maximum int
	window  time.Duration
	now     func() time.Time
}

func NewLimiter(maximum int, window time.Duration, now func() time.Time) *Limiter {
	return &Limiter{entries: make(map[string]limitWindow), maximum: maximum, window: window, now: now}
}
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, exists := l.entries[key]
	if !exists && len(l.entries) >= 4096 {
		for k, v := range l.entries {
			if now.Sub(v.start) >= l.window {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= 4096 {
			return false
		}
	}
	if !exists || now.Sub(e.start) >= l.window {
		e = limitWindow{start: now}
	}
	if e.count >= l.maximum {
		return false
	}
	e.count++
	l.entries[key] = e
	return true
}
func (m *Management) limit(w http.ResponseWriter, r *http.Request, l *Limiter) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if !l.Allow(host) {
		w.Header().Set("Retry-After", "60")
		JSON(w, 429, map[string]string{"error": "rate_limited"})
		return false
	}
	return true
}
