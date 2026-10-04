package auth_test

import (
	"switchyard/internal/auth"
	"testing"
)

func TestCredentialValidation(t *testing.T) {
	for _, tt := range []struct {
		password string
		valid    bool
	}{{"short", false}, {"a-long-demo-password", true}, {string(make([]byte, 73)), false}} {
		if (auth.ValidatePassword(tt.password) == nil) != tt.valid {
			t.Fatalf("password length %d validity mismatch", len(tt.password))
		}
	}
	email, err := auth.NormalizeEmail("  Demo@EXAMPLE.test ")
	if err != nil || email != "demo@example.test" {
		t.Fatalf("email=%s err=%v", email, err)
	}
	for _, email := range []string{"bad", "User <user@example.test>", ""} {
		if _, err := auth.NormalizeEmail(email); err == nil {
			t.Errorf("accepted malformed email %q", email)
		}
	}
	if auth.ValidRole("owner") {
		t.Fatal("accepted undefined role")
	}
}
