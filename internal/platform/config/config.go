// Package config loads explicit process configuration without global mutable state.
package config

import (
	"errors"
	"net"
	"strconv"
)

type Config struct {
	HTTPAddr     string
	DatabaseURL  string
	MaxConns     int32
	CacheEnabled bool
	RedisURL     string
}

func Load(getenv func(string) string) (Config, error) {
	c := Config{HTTPAddr: getenv("HTTP_ADDR"), DatabaseURL: getenv("DATABASE_URL"), MaxConns: 10, CacheEnabled: true, RedisURL: getenv("REDIS_URL")}
	if s := getenv("CACHE_ENABLED"); s != "" {
		value, err := strconv.ParseBool(s)
		if err != nil {
			return Config{}, errors.New("CACHE_ENABLED must be a boolean")
		}
		c.CacheEnabled = value
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
	if s := getenv("DB_MAX_CONNS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			return Config{}, errors.New("DB_MAX_CONNS must be between 1 and 100")
		}
		c.MaxConns = int32(n)
	}
	return c, nil
}
