package main

import (
	"strings"
	"testing"
)

func setRequiredConfig(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"WEBHOOK_LISTEN", "OPS_LISTEN", "DOMAIN_FILTER", "DRY_RUN",
		"CACHE_TTL", "LOG_LEVEL", "LOG_FORMAT",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("PORKBUN_API_KEY", "key")
	t.Setenv("PORKBUN_SECRET_API_KEY", "secret")
	t.Setenv("PORKBUN_DOMAIN", "example.com")
}

func TestLoadConfigDefaults(t *testing.T) {
	setRequiredConfig(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookListen != "127.0.0.1:8888" {
		t.Fatalf("WebhookListen = %q", cfg.WebhookListen)
	}
	if cfg.OpsListen != ":8080" || cfg.CacheTTL.String() != "1m0s" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.DryRun {
		t.Fatal("DryRun defaulted to true")
	}
}

func TestLoadConfigRejectsUnsafeOrInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{name: "mistyped dry run", key: "DRY_RUN", value: "treu", wantErr: "DRY_RUN must be a boolean"},
		{name: "negative cache", key: "CACHE_TTL", value: "-1s", wantErr: "must not be negative"},
		{name: "bad log level", key: "LOG_LEVEL", value: "verbose", wantErr: "LOG_LEVEL"},
		{name: "bad log format", key: "LOG_FORMAT", value: "yaml", wantErr: "LOG_FORMAT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredConfig(t)
			t.Setenv(tt.key, tt.value)
			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("loadConfig() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigRequiresCredentialsAndDomain(t *testing.T) {
	t.Setenv("PORKBUN_API_KEY", "")
	t.Setenv("PORKBUN_SECRET_API_KEY", "")
	t.Setenv("PORKBUN_DOMAIN", "")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() succeeded without required configuration")
	}
}
