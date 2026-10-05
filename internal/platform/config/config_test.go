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

func TestRetentionSettings(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://localhost/switchyard"}
	defaults, err := config.Load(func(k string) string { return base[k] })
	if err != nil || defaults.WorkerRetentionEnabled || defaults.RawRetentionDays != 7 || defaults.SummaryRetentionDays != 90 {
		t.Fatal("retention defaults", defaults, err)
	}
	for _, entry := range []struct{ key, value string }{
		{"WORKER_RETENTION_ENABLED", "maybe"}, {"RAW_RETENTION_DAYS", "1"}, {"RAW_RETENTION_DAYS", "8"}, {"RAW_RETENTION_DAYS", "NaN"}, {"SUMMARY_RETENTION_DAYS", "7"}, {"SUMMARY_RETENTION_DAYS", "366"}} {
		if _, err := config.Load(func(k string) string {
			if k == entry.key {
				return entry.value
			}
			return base[k]
		}); err == nil {
			t.Fatal("unsupported retention setting", entry)
		}
	}
	cfg, err := config.Load(func(k string) string {
		switch k {
		case "WORKER_RETENTION_ENABLED":
			return "true"
		case "RAW_RETENTION_DAYS":
			return "2"
		case "SUMMARY_RETENTION_DAYS":
			return "8"
		}
		return base[k]
	})
	if err != nil || !cfg.WorkerRetentionEnabled || cfg.RawRetentionDays != 2 || cfg.SummaryRetentionDays != 8 {
		t.Fatal("valid retention settings", cfg, err)
	}
}
