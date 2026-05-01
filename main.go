// Command external-dns-porkbun-webhook is the entry point for the binary.
//
// Configuration is from environment variables:
//
//	PORKBUN_API_KEY            (required)  - Porkbun API key
//	PORKBUN_SECRET_API_KEY     (required)  - Porkbun secret API key
//	PORKBUN_DOMAIN             (required)  - the apex zone, e.g. "example.com"
//	WEBHOOK_LISTEN             (default ":8888")  - external-dns webhook server
//	OPS_LISTEN                 (default ":8080")  - healthz/readyz/metrics
//	DOMAIN_FILTER              (default = PORKBUN_DOMAIN, comma-separated for multiple)
//	DRY_RUN                    (default false)
//	CACHE_TTL                  (default "1m")     - cache for record retrieval
//	LOG_LEVEL                  (default "info")
//	LOG_FORMAT                 (default "text", or "json")
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/external-dns/endpoint"

	"github.com/mattgmoser/external-dns-porkbun-webhook/provider"
	"github.com/mattgmoser/external-dns-porkbun-webhook/webhook"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	if err := run(); err != nil {
		log.WithError(err).Fatal("fatal")
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	configureLogging(cfg)

	log.WithFields(log.Fields{
		"version":       Version,
		"domain":        cfg.Domain,
		"domain_filter": cfg.DomainFilter,
		"dry_run":       cfg.DryRun,
		"webhook_addr":  cfg.WebhookListen,
		"ops_addr":      cfg.OpsListen,
		"cache_ttl":     cfg.CacheTTL,
	}).Info("starting external-dns-porkbun-webhook")

	prov, err := provider.New(provider.Config{
		APIKey:       cfg.APIKey,
		SecretAPIKey: cfg.SecretAPIKey,
		Domain:       cfg.Domain,
		DomainFilter: endpoint.NewDomainFilter(cfg.DomainFilter),
		DryRun:       cfg.DryRun,
		CacheTTL:     cfg.CacheTTL,
	})
	if err != nil {
		return fmt.Errorf("provider init: %w", err)
	}

	// Optional pre-flight credential check; non-fatal so the readiness probe can do its job.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pingCancel()
	if err := prov.Ping(pingCtx); err != nil {
		log.WithError(err).Warn("pre-flight credential check failed; readiness will reflect this")
	} else {
		log.Info("credentials ok")
	}

	srv := webhook.New(webhook.Config{
		Provider: prov,
		Addr:     cfg.WebhookListen,
		OpsAddr:  cfg.OpsListen,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// ----- config -----

type config struct {
	APIKey        string
	SecretAPIKey  string
	Domain        string
	DomainFilter  []string
	WebhookListen string
	OpsListen     string
	DryRun        bool
	CacheTTL      time.Duration
	LogLevel      string
	LogFormat     string
}

func loadConfig() (*config, error) {
	c := &config{
		WebhookListen: getenv("WEBHOOK_LISTEN", ":8888"),
		OpsListen:     getenv("OPS_LISTEN", ":8080"),
		LogLevel:      getenv("LOG_LEVEL", "info"),
		LogFormat:     getenv("LOG_FORMAT", "text"),
	}
	c.APIKey = os.Getenv("PORKBUN_API_KEY")
	c.SecretAPIKey = os.Getenv("PORKBUN_SECRET_API_KEY")
	c.Domain = strings.TrimSpace(os.Getenv("PORKBUN_DOMAIN"))

	if c.APIKey == "" || c.SecretAPIKey == "" {
		return nil, fmt.Errorf("PORKBUN_API_KEY and PORKBUN_SECRET_API_KEY must be set")
	}
	if c.Domain == "" {
		return nil, fmt.Errorf("PORKBUN_DOMAIN must be set (apex zone, e.g. example.com)")
	}

	if df := os.Getenv("DOMAIN_FILTER"); df != "" {
		for _, f := range strings.Split(df, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				c.DomainFilter = append(c.DomainFilter, f)
			}
		}
	}

	c.DryRun = boolEnv("DRY_RUN", false)

	ttlStr := getenv("CACHE_TTL", "1m")
	d, err := time.ParseDuration(ttlStr)
	if err != nil {
		return nil, fmt.Errorf("CACHE_TTL: %w", err)
	}
	c.CacheTTL = d
	return c, nil
}

func configureLogging(c *config) {
	if lvl, err := log.ParseLevel(c.LogLevel); err == nil {
		log.SetLevel(lvl)
	}
	switch strings.ToLower(c.LogFormat) {
	case "json":
		log.SetFormatter(&log.JSONFormatter{})
	default:
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func boolEnv(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
