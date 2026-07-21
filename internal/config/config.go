// Package config loads and validates devlogd's YAML configuration and applies
// 12-factor environment overrides (DEVLOG_* variables win over the file).
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration lets values like "24h" or "500ms" be written in YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	*d = Duration(dur)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// TLS points at PEM material. A non-empty ClientCAFile switches the listener
// to mutual TLS: clients must present a certificate signed by that CA.
type TLS struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// AutoStart controls whether devlogd spawns its own local Redis or MinIO
// process at startup when nothing answers at the configured address yet.
// Disabled by default: an operator opts in for a single-station/appliance
// deployment that isn't handed an already-running Redis/MinIO. It only ever
// acts once, at startup — devlogd does not supervise or respawn either
// process if it dies later while the service is running.
type AutoStart struct {
	Enabled bool     `yaml:"enabled"`
	BinPath string   `yaml:"bin_path"`
	DataDir string   `yaml:"data_dir"`
	Timeout Duration `yaml:"timeout"`
}

type Config struct {
	MQTT struct {
		Listen string `yaml:"listen"`
		TLS    TLS    `yaml:"tls"`
	} `yaml:"mqtt"`
	GRPC struct {
		Listen string `yaml:"listen"`
		TLS    TLS    `yaml:"tls"`
	} `yaml:"grpc"`
	HTTP struct {
		Listen string `yaml:"listen"`
	} `yaml:"http"`
	Redis struct {
		Addr         string    `yaml:"addr"`
		Password     string    `yaml:"password"`
		HotRetention Duration  `yaml:"hot_retention"`
		AutoStart    AutoStart `yaml:"auto_start"`
	} `yaml:"redis"`
	S3 struct {
		Endpoint  string    `yaml:"endpoint"`
		AccessKey string    `yaml:"access_key"`
		SecretKey string    `yaml:"secret_key"`
		Bucket    string    `yaml:"bucket"`
		UseTLS    bool      `yaml:"use_tls"`
		AutoStart AutoStart `yaml:"auto_start"`
	} `yaml:"s3"`
	Signing struct {
		KeyFile string `yaml:"key_file"`
		KeyID   string `yaml:"key_id"`
	} `yaml:"signing"`
	License struct {
		IssuerPubFile string   `yaml:"issuer_pub_file"`
		Mode          string   `yaml:"mode"` // "offline" or "online"
		ServerURL     string   `yaml:"server_url"`
		ServerCAFile  string   `yaml:"server_ca_file"`
		Grace         Duration `yaml:"grace"`
	} `yaml:"license"`
	Archive struct {
		FlushInterval   Duration `yaml:"flush_interval"`
		MaxBatchBytes   int      `yaml:"max_batch_bytes"`
		MaxBatchEntries int      `yaml:"max_batch_entries"`
	} `yaml:"archive"`
	Query struct {
		MaxResults      int      `yaml:"max_results"`
		DefaultLookback Duration `yaml:"default_lookback"`
	} `yaml:"query"`
	Log struct {
		Level string `yaml:"level"`
	} `yaml:"log"`
}

func defaults() *Config {
	c := &Config{}
	c.MQTT.Listen = ":8883"
	c.GRPC.Listen = ":9443"
	c.HTTP.Listen = ":9090"
	c.Redis.Addr = "localhost:6379"
	c.Redis.HotRetention = Duration(24 * time.Hour)
	c.Redis.AutoStart = AutoStart{BinPath: "redis-server", DataDir: "data/redis", Timeout: Duration(15 * time.Second)}
	c.S3.Bucket = "devlog"
	c.S3.AutoStart = AutoStart{BinPath: "minio", DataDir: "data/minio", Timeout: Duration(15 * time.Second)}
	c.Signing.KeyID = "devlogd-1"
	c.License.Mode = "offline"
	c.License.Grace = Duration(72 * time.Hour)
	c.Archive.FlushInterval = Duration(60 * time.Second)
	c.Archive.MaxBatchBytes = 8 << 20
	c.Archive.MaxBatchEntries = 5000
	c.Query.MaxResults = 10000
	c.Query.DefaultLookback = Duration(7 * 24 * time.Hour)
	c.Log.Level = "info"
	return c
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := defaults()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyEnv()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func (c *Config) applyEnv() {
	overrides := map[string]*string{
		"DEVLOG_MQTT_LISTEN":        &c.MQTT.Listen,
		"DEVLOG_GRPC_LISTEN":        &c.GRPC.Listen,
		"DEVLOG_HTTP_LISTEN":        &c.HTTP.Listen,
		"DEVLOG_REDIS_ADDR":         &c.Redis.Addr,
		"DEVLOG_REDIS_PASSWORD":     &c.Redis.Password,
		"DEVLOG_S3_ENDPOINT":        &c.S3.Endpoint,
		"DEVLOG_S3_ACCESS_KEY":      &c.S3.AccessKey,
		"DEVLOG_S3_SECRET_KEY":      &c.S3.SecretKey,
		"DEVLOG_S3_BUCKET":          &c.S3.Bucket,
		"DEVLOG_LICENSE_MODE":       &c.License.Mode,
		"DEVLOG_LICENSE_SERVER_URL": &c.License.ServerURL,
		"DEVLOG_LOG_LEVEL":          &c.Log.Level,
	}
	for key, dst := range overrides {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
}

func (c *Config) validate() error {
	required := map[string]string{
		"mqtt.tls.cert_file":      c.MQTT.TLS.CertFile,
		"mqtt.tls.key_file":       c.MQTT.TLS.KeyFile,
		"grpc.tls.cert_file":      c.GRPC.TLS.CertFile,
		"grpc.tls.key_file":       c.GRPC.TLS.KeyFile,
		"s3.endpoint":             c.S3.Endpoint,
		"s3.bucket":               c.S3.Bucket,
		"signing.key_file":        c.Signing.KeyFile,
		"signing.key_id":          c.Signing.KeyID,
		"license.issuer_pub_file": c.License.IssuerPubFile,
	}
	for name, v := range required {
		if v == "" {
			return fmt.Errorf("%s must be set", name)
		}
	}
	switch c.License.Mode {
	case "offline":
	case "online":
		if c.License.ServerURL == "" {
			return fmt.Errorf("license.server_url must be set when license.mode is online")
		}
	default:
		return fmt.Errorf("license.mode must be offline or online, got %q", c.License.Mode)
	}
	return nil
}
