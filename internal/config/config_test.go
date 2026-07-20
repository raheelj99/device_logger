package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A minimal config that satisfies validate(); tests tweak it per case.
const validYAML = `
mqtt:
  listen: ":8883"
  tls:
    cert_file: /x/server.crt
    key_file: /x/server.key
grpc:
  tls:
    cert_file: /x/server.crt
    key_file: /x/server.key
s3:
  endpoint: "localhost:9000"
  bucket: devlog
signing:
  key_file: /x/signing.key
  key_id: k1
license:
  issuer_pub_file: /x/issuer.pub
  mode: offline
redis:
  hot_retention: 12h
`

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "devlogd.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAppliesDefaultsAndParsesDurations(t *testing.T) {
	cfg, err := Load(writeCfg(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	// Value from the file, parsed via the custom Duration unmarshaler.
	if cfg.Redis.HotRetention.Std() != 12*time.Hour {
		t.Fatalf("hot_retention = %v, want 12h", cfg.Redis.HotRetention.Std())
	}
	// Untouched keys fall back to defaults().
	if cfg.GRPC.Listen != ":9443" {
		t.Fatalf("grpc.listen default lost: %q", cfg.GRPC.Listen)
	}
	if cfg.Query.MaxResults != 10000 {
		t.Fatalf("query.max_results default lost: %d", cfg.Query.MaxResults)
	}
}

func TestEnvOverridesWinOverFile(t *testing.T) {
	t.Setenv("DEVLOG_REDIS_ADDR", "redis.internal:6380")
	t.Setenv("DEVLOG_LOG_LEVEL", "debug")
	cfg, err := Load(writeCfg(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Addr != "redis.internal:6380" {
		t.Fatalf("env override ignored: %q", cfg.Redis.Addr)
	}
	if cfg.Log.Level != "debug" {
		t.Fatalf("log level override ignored: %q", cfg.Log.Level)
	}
}

func TestValidateRejectsMissingRequired(t *testing.T) {
	// Drop the signing key file — validate() must fail fast.
	body := `
mqtt:
  tls: {cert_file: /x/c, key_file: /x/k}
grpc:
  tls: {cert_file: /x/c, key_file: /x/k}
s3: {endpoint: "e", bucket: b}
signing: {key_id: k1}
license: {issuer_pub_file: /x/i, mode: offline}
`
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatal("expected error for missing signing.key_file")
	}
}

func TestValidateOnlineModeRequiresServerURL(t *testing.T) {
	body := validYAML + "\nlicense:\n  issuer_pub_file: /x/i\n  mode: online\n"
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("expected error: online mode without server_url")
	}
}

func TestValidateRejectsUnknownMode(t *testing.T) {
	body := validYAML + "\nlicense:\n  issuer_pub_file: /x/i\n  mode: sideways\n"
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("expected error for unknown license.mode")
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	body := validYAML + "\narchive:\n  flush_interval: not-a-duration\n"
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("expected duration parse error")
	}
}
