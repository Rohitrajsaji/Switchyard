//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"golang.org/x/crypto/bcrypt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchyard/internal/auth"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/testutil"
	httpapi "switchyard/internal/transport/http"
	"switchyard/migrations"
)

func TestManagementCSRFScopesCookiesAndSecretSafeLogs(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	// This fixture tests transport permissions, not bcrypt throughput under -race.
	// Production password hashing remains cost 12 and is covered in auth integration.
	hashBytes, err := bcrypt.GenerateFromPassword([]byte("a-good-demo-password"), bcrypt.MinCost)
	hash := string(hashBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test',$1,'admin')`, hash); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	m, err := httpapi.NewManagement(pool, logger, "http://localhost:3000", false)
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.New(logger, func(context.Context) error { return nil }, m.Register)
	request := func(method, path, body, origin, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	login := request("POST", "/v1/session", `{"email":"admin@example.test","password":"a-good-demo-password"}`, "http://localhost:3000", "", nil)
	if login.Code != 200 {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	var session auth.Session
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing session cookie")
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie options")
	}
	for _, tt := range []struct {
		origin, csrf string
		cookie       *http.Cookie
		expected     int
	}{
		{"http://localhost:3000", "", cookie, 403}, {"https://evil.example", session.CSRF, cookie, 403}, {"http://localhost:3000", session.CSRF, nil, 401},
	} {
		w := request("POST", "/v1/projects", `{"name":"Marketplace"}`, tt.origin, tt.csrf, tt.cookie)
		if w.Code != tt.expected {
			t.Fatalf("csrf/auth expected=%d got=%d", tt.expected, w.Code)
		}
	}
	created := request("POST", "/v1/projects", `{"name":"Marketplace"}`, "http://localhost:3000", session.CSRF, cookie)
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	if w := request("GET", "/v1/projects", "", "", "", cookie); w.Code != 200 {
		t.Fatal("project read failed")
	}
	if w := request("DELETE", "/v1/session", "", "http://localhost:3000", session.CSRF, cookie); w.Code != 204 {
		t.Fatal("logout failed")
	}
	if w := request("GET", "/v1/session", "", "", "", cookie); w.Code != 401 {
		t.Fatal("session survived logout")
	}
	for _, secret := range []string{cookie.Value, session.CSRF, "a-good-demo-password", hash} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret present in request logs")
		}
	}
}
