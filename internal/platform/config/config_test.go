package config_test

import (
	"testing"

	"switchyard/internal/platform/config"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{"missing database", nil, true},
		{"valid", map[string]string{"DATABASE_URL": "postgres://localhost/switchyard"}, false},
		{"invalid pool", map[string]string{"DATABASE_URL": "postgres://localhost/switchyard", "DB_MAX_CONNS": "0"}, true},
		{"invalid address", map[string]string{"DATABASE_URL": "postgres://localhost/switchyard", "HTTP_ADDR": "bad"}, true},
		{"invalid cache", map[string]string{"DATABASE_URL": "postgres://localhost/switchyard", "CACHE_ENABLED": "maybe"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Load(func(k string) string { return tt.env[k] })
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && (cfg.MaxConns < 1 || cfg.HTTPAddr == "") {
				t.Fatal("invalid defaults")
			}
		})
	}
}
