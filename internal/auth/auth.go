package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
	"switchyard/internal/platform/identity"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrInvalid      = errors.New("invalid input")
	ErrConflict     = errors.New("conflict")
)

type Actor struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}
type Session struct {
	Actor     Actor     `json:"user"`
	Token     string    `json:"-"`
	CSRF      string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Application struct {
	ID            string
	ProjectID     string
	EnvironmentID string
	Permissions   []string
}
type Service struct {
	pool  *pgxpool.Pool
	dummy []byte
}

func New(pool *pgxpool.Pool) (*Service, error) {
	dummy, err := bcrypt.GenerateFromPassword([]byte(identity.New("dummy_")), 12)
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, dummy: dummy}, nil
}

func ValidatePassword(password string) error {
	if len(password) < 12 || len(password) > 72 {
		return ErrInvalid
	}
	return nil
}
func PasswordHash(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(h), err
}
func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 254 {
		return "", ErrInvalid
	}
	return email, nil
}
func ValidRole(role string) bool { return role == "viewer" || role == "developer" || role == "admin" }

func (s *Service) Login(ctx context.Context, email, password string) (Session, error) {
	if len(password) > 72 {
		return Session{}, ErrUnauthorized
	}
	email = strings.ToLower(strings.TrimSpace(email))
	var actor Actor
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT id,email,role,password_hash FROM users WHERE email=$1 AND active`, email).Scan(&actor.ID, &actor.Email, &actor.Role, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(s.dummy, []byte(password))
		return Session{}, ErrUnauthorized
	}
	if err != nil {
		return Session{}, err
	}
	if len(password) > 72 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return Session{}, ErrUnauthorized
	}
	token := identity.New("sws_")
	session := Session{Actor: actor, Token: token, CSRF: csrfFor(token), ExpiresAt: time.Now().UTC().Add(24 * time.Hour)}
	_, err = s.pool.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,$4)`, identity.Hash(session.Token), actor.ID, identity.Hash(session.CSRF), session.ExpiresAt)
	return session, err
}

func (s *Service) Authenticate(ctx context.Context, token string) (Actor, error) {
	if len(token) != 68 || !strings.HasPrefix(token, "sws_") {
		return Actor{}, ErrUnauthorized
	}
	var actor Actor
	err := s.pool.QueryRow(ctx, `SELECT u.id,u.email,u.role FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND u.active`, identity.Hash(token)).Scan(&actor.ID, &actor.Email, &actor.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Actor{}, ErrUnauthorized
	}
	return actor, err
}
func (s *Service) VerifyCSRF(ctx context.Context, token, csrf string) error {
	if len(csrf) != 68 {
		return ErrForbidden
	}
	var expected []byte
	err := s.pool.QueryRow(ctx, `SELECT csrf_hash FROM sessions WHERE token_hash=$1 AND expires_at>now()`, identity.Hash(token)).Scan(&expected)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(expected, identity.Hash(csrf)) != 1 {
		return ErrForbidden
	}
	return nil
}
func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, identity.Hash(token))
	return err
}
func csrfFor(token string) string {
	return "swc_" + fmt.Sprintf("%x", identity.Hash("switchyard-csrf-v1:"+token))
}
func (s *Service) Current(ctx context.Context, token string) (Session, error) {
	actor, err := s.Authenticate(ctx, token)
	if err != nil {
		return Session{}, err
	}
	var expiry time.Time
	if err := s.pool.QueryRow(ctx, `SELECT expires_at FROM sessions WHERE token_hash=$1`, identity.Hash(token)).Scan(&expiry); err != nil {
		return Session{}, err
	}
	return Session{Actor: actor, CSRF: csrfFor(token), ExpiresAt: expiry}, nil
}
func (s *Service) AuthenticateApplication(ctx context.Context, token, projectID, environmentID, permission string) (Application, error) {
	return AuthorizeApplication(ctx, s.pool, token, projectID, environmentID, permission)
}

// AuthorizeApplication allows ingestion to check scope inside its write transaction.
func AuthorizeApplication(ctx context.Context, db Queryer, token, projectID, environmentID, permission string) (Application, error) {
	if len(token) != 68 || !strings.HasPrefix(token, "swk_") {
		return Application{}, ErrUnauthorized
	}
	var a Application
	err := db.QueryRow(ctx, `SELECT id,project_id,environment_id,permissions FROM application_keys WHERE token_hash=$1 AND revoked_at IS NULL`, identity.Hash(token)).Scan(&a.ID, &a.ProjectID, &a.EnvironmentID, &a.Permissions)
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrUnauthorized
	}
	if err != nil {
		return Application{}, err
	}
	if a.ProjectID != projectID || a.EnvironmentID != environmentID {
		return Application{}, ErrForbidden
	}
	for _, p := range a.Permissions {
		if p == permission {
			return a, nil
		}
	}
	return Application{}, ErrForbidden
}
