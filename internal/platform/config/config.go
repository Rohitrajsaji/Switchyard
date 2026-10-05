// Package config loads explicit process configuration without global mutable state.
package config

import (
	"errors"
	"net"
	"strconv"
)

type Config struct {
	HTTPAddr               string
	GRPCAddr               string
	DatabaseURL            string
	MaxConns               int32
	CacheEnabled           bool
	RedisURL               string
	WorkerRetentionEnabled bool
	RawRetentionDays       int
	SummaryRetentionDays   int
}

func Load(getenv func(string) string) (Config, error) {
	c := Config{HTTPAddr: getenv("HTTP_ADDR"), GRPCAddr: getenv("GRPC_ADDR"), DatabaseURL: getenv("DATABASE_URL"), MaxConns: 10, CacheEnabled: true, RedisURL: getenv("REDIS_URL"), RawRetentionDays: 7, SummaryRetentionDays: 90}
	if s := getenv("CACHE_ENABLED"); s != "" {
		value, err := strconv.ParseBool(s)
		if err != nil {
			return Config{}, errors.New("CACHE_ENABLED must be a boolean")
		}
		c.CacheEnabled = value
	}
	if s := getenv("WORKER_RETENTION_ENABLED"); s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return Config{}, errors.New("WORKER_RETENTION_ENABLED must be a boolean")
		}
		c.WorkerRetentionEnabled = v
	}
	for _, setting := range []struct {
		name     string
		target   *int
		min, max int
	}{
		{"RAW_RETENTION_DAYS", &c.RawRetentionDays, 2, 7}, {"SUMMARY_RETENTION_DAYS", &c.SummaryRetentionDays, 8, 365}} {
		if s := getenv(setting.name); s != "" {
			v, err := strconv.Atoi(s)
			if err != nil || v < setting.min || v > setting.max {
				return Config{}, errors.New(setting.name + " is outside its supported retention range")
			}
			*setting.target = v
		}
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return Config{}, errors.New("HTTP_ADDR must be host:port")
	}
	if c.GRPCAddr == "" {
		c.GRPCAddr = "127.0.0.1:9090"
	}
	if _, _, err := net.SplitHostPort(c.GRPCAddr); err != nil {
		return Config{}, errors.New("GRPC_ADDR must be host:port")
	}
	if s := getenv("DB_MAX_CONNS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			return Config{}, errors.New("DB_MAX_CONNS must be between 1 and 100")
		}
		c.MaxConns = int32(n)
	}
	return c, nil
}
