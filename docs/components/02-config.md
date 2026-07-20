# Component 02 — `internal/config`

> **Role:** load and validate devlogd's YAML configuration, apply environment
> overrides, and build TLS settings. | **Source:** `internal/config/config.go`,
> `internal/config/tls.go`

**Where this sits in the journey:** your first real Go package. It touches the
concepts you will use in every other component — structs, methods, interfaces,
maps, pointers, and Go's error model — in a small, self-contained package with
no concurrency to distract you. Prerequisite: [01 — proto contract](01-proto-contract.md)
(so you know what "generated types" are); everything else you'll learn here.

## What you'll master

- **Go:** package declaration & doc comments; imports; **defined types** and
  methods on them; **value vs pointer receivers**; **interfaces via implicit
  satisfaction** (`yaml.Unmarshaler`); structs, **nested anonymous structs**,
  and **struct tags**; **zero values**; maps; pointers and dereferencing;
  **error values** and wrapping with `%w`; `switch` with empty cases; the
  `crypto/tls` and `crypto/x509` types.
- **Domain:** 12-factor configuration, fail-fast validation, TLS vs mutual TLS.

---

## 1. Orientation

Every long-lived service needs a front door for its settings. In devlogd that
door is `config.Load(path)`: it reads a YAML file, lets `DEVLOG_*` environment
variables override any value, checks that the security-critical fields are
present and sane, and returns a `*Config` the rest of the program is built from.
If anything is wrong, it returns an **error** and the process exits before it
ever opens a socket — the *fail-fast* discipline.

It also owns TLS construction (`ServerTLS`, `ClientTLS`), because "what
certificate do we present and who do we trust" is a configuration question.

---

## 2. Guided code walkthrough

### 2.1 The package clause and doc comment

```go
// Package config loads and validates devlogd's YAML configuration and applies
// 12-factor environment overrides (DEVLOG_* variables win over the file).
package config
```

> **Go concept — packages.** A Go program is a tree of *packages*; every `.go`
> file starts with `package <name>`. All files in one directory must share the
> same package name and together form one unit — there are no headers and no
> `.cpp/.h` split. The comment immediately above `package` is the **package doc
> comment**; `go doc` and pkg.go.dev render it. Unlike a C++ namespace, a Go
> package is also the unit of *compilation* and *visibility* (see §2.3).

### 2.2 Imports

```go
import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)
```

> **Go concept — imports & the two groups.** You import *packages*, referenced
> by the last path element (`yaml.Unmarshal`, `time.Duration`). The blank line
> splits the **standard library** (top) from **third-party** modules (bottom) —
> `gofmt`/`goimports` maintain this convention automatically. An imported-but-
> unused package is a **compile error**, not a warning: Go refuses to let dead
> imports rot. `gopkg.in/yaml.v3` is resolved to a specific version by
> `go.mod`/`go.sum` (the module system from doc 01).

### 2.3 A defined type and its methods

```go
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
```

`time.Duration` is itself an `int64` counting nanoseconds. YAML, though, gives
us the *string* `"24h"`. We want the YAML library to parse that string for us
automatically — so we wrap `time.Duration` in our own type and teach it how to
unmarshal.

> **Go concept — defined types.** `type Duration time.Duration` creates a
> brand-new type with the same underlying representation as `time.Duration` but
> a **distinct identity**. This is not a C++ `typedef`/`using` alias (which
> creates a synonym); it is closer to a strong `enum class`-style newtype. You
> can attach your own methods to it, and it will not accidentally mix with the
> original type without an explicit conversion (`Duration(dur)` /
> `time.Duration(d)`).

> **Go concept — methods & receivers.** A method is a function with a
> **receiver** written before the name: `func (d *Duration) UnmarshalYAML(...)`.
> Here `d` is like `this`, but it is *explicit* and *named by you*. Methods can
> be attached to any type **defined in the same package** — including
> non-structs like `Duration` — which is why we can give an integer type a
> parsing method. That is impossible in C++, where methods must live inside the
> class body.

> **Go concept — value vs pointer receivers.** `UnmarshalYAML` uses `*Duration`
> because it must **mutate** the receiver (`*d = ...`); a value receiver would
> get a copy and the change would be lost. `Std()` uses a value receiver
> `Duration` because it only *reads*. Rule of thumb: pointer receiver if you
> mutate or the struct is large; otherwise value. Keep it **consistent** across
> a type's methods.

> **Go concept — interfaces are satisfied implicitly.** Nowhere does the code
> say "Duration implements yaml.Unmarshaler". The `yaml` library declares
> `interface { UnmarshalYAML(*yaml.Node) error }`; because our type happens to
> have a method with that exact signature, it *automatically* satisfies the
> interface. This is **structural typing** — no `implements` keyword, no
> inheritance. Contrast C++ where you'd inherit an abstract base or specialize a
> template. Go's approach means you can make *someone else's* type satisfy
> *your* interface without touching their code.

> **Go concept — errors are values, and `%w` wraps them.** Go has no
> exceptions. Functions that can fail return an `error` as their last result;
> the caller checks it. `fmt.Errorf("... %w", err)` builds a new error that
> **wraps** the original, so callers up the stack can later test it with
> `errors.Is`/`errors.As` (you'll use those in doc 06). `%q` prints a
> double-quoted, escaped string — invaluable in error messages.

### 2.4 The `Config` struct — nesting and tags

```go
type TLS struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

type Config struct {
	MQTT struct {
		Listen string `yaml:"listen"`
		TLS    TLS    `yaml:"tls"`
	} `yaml:"mqtt"`
	// … grpc, http, redis, s3, signing, license, archive, query, log …
	Redis struct {
		Addr         string   `yaml:"addr"`
		Password     string   `yaml:"password"`
		HotRetention Duration `yaml:"hot_retention"`
	} `yaml:"redis"`
}
```

> **Go concept — structs.** A `struct` is an aggregate of named fields, like a
> C++ `struct`/`class` with only data. Fields are laid out in memory in order,
> value-typed by default (assigning a struct copies it). There are no
> constructors, destructors, or inheritance.

> **Go concept — nested anonymous structs.** `MQTT struct { … }` declares a
> field whose type is an *unnamed* struct written inline. It mirrors the YAML's
> shape (`mqtt: { listen: …, tls: … }`) without inventing a top-level `MQTTConf`
> type for each section. Access is `cfg.MQTT.Listen`. Reuse (like `TLS`, used by
> both `mqtt` and `grpc`) gets a named type instead.

> **Go concept — struct tags.** The backtick string after a field —
> `` `yaml:"cert_file"` `` — is a **struct tag**: metadata read at runtime via
> reflection. The `yaml` decoder uses it to map the YAML key `cert_file` to the
> Go field `CertFile`. Tags are how Go does declarative (de)serialization; the
> same mechanism drives `json:"…"` (doc 05). The tag is *just a string* — the
> compiler doesn't check it, so a typo silently breaks a mapping (a classic
> gotcha).

> **Go concept — exported vs unexported (capitalization).** `CertFile` starts
> with a capital letter, so it is **exported** (visible outside the package) —
> the equivalent of `public`. A lowercase name (`skew` later) is
> **unexported** (package-private). Visibility is a property of the *identifier's
> first letter*, not an access keyword. The YAML decoder can only set exported
> fields, which is why every configurable field is capitalized.

### 2.5 Zero values and the defaults constructor

```go
func defaults() *Config {
	c := &Config{}
	c.MQTT.Listen = ":8883"
	c.Redis.HotRetention = Duration(24 * time.Hour)
	c.Query.MaxResults = 10000
	c.License.Mode = "offline"
	// …
	return c
}
```

> **Go concept — zero values.** `&Config{}` allocates a `Config` with every
> field set to its **zero value**: `""` for strings, `0` for numbers, `false`
> for bools, `nil` for pointers/maps/slices. There is no "uninitialized memory"
> in Go — a freshly declared variable is always usable. `defaults()` then
> overrides the fields where zero is the *wrong* default (a listener of `""` is
> useless; `":8883"` is right).

> **Go concept — `&T{}` and no `new`/`delete`.** `&Config{}` creates a value and
> takes its address in one step, yielding a `*Config`. It is safe to return a
> pointer to a local — Go's **escape analysis** moves it to the heap and the
> **garbage collector** frees it later. There is no `delete`, no ownership
> ceremony, no dangling pointer.

### 2.6 `Load` — the orchestrator

```go
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
```

> **Go concept — multiple return values.** `Load` returns `(*Config, error)`.
> Returning a result *and* an error together is the core Go idiom; there is no
> out-parameter and no exception. The universal shape is:
> `x, err := f(); if err != nil { return …, err }`.

> **Go concept — `:=` vs `=` and shadowing in `if`.** `raw, err := …` declares
> and assigns (short variable declaration). `if err := f(); err != nil { … }`
> declares `err` **scoped to the `if`** — a very common Go pattern that keeps
> throwaway errors out of the surrounding scope. Note the earlier `err` and
> these `if`-scoped `err`s are different variables; that's deliberate, not a bug.

Notice the pipeline: **defaults → file → environment → validate**. Each stage
can only tighten or override the previous, and validation is *last* so it sees
the final, merged values.

### 2.7 `applyEnv` — a map of pointers

```go
func (c *Config) applyEnv() {
	overrides := map[string]*string{
		"DEVLOG_MQTT_LISTEN":   &c.MQTT.Listen,
		"DEVLOG_REDIS_ADDR":    &c.Redis.Addr,
		"DEVLOG_LOG_LEVEL":     &c.Log.Level,
		// … one entry per overridable field …
	}
	for key, dst := range overrides {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
}
```

> **Go concept — maps.** `map[string]*string` is a hash map from string keys to
> `*string` values (Go's built-in generic dictionary). Literal syntax fills it
> inline. This is an elegant trick: the map's *values are pointers to the actual
> config fields*, so writing through the pointer edits the struct in place.

> **Go concept — `range` and the comma-ok idiom.** `for key, dst := range
> overrides` iterates a map, yielding key and value each turn (map order is
> **randomized** by design — never rely on it). `v, ok := os.LookupEnv(key)`
> returns the value **and** a boolean saying whether the variable was set; the
> "comma-ok" idiom lets you distinguish "unset" from "set to empty". Only when
> `ok` do we override, so an *unset* variable leaves the file value intact.

> **Go concept — dereferencing.** `*dst = v` writes through the pointer to the
> field it points at. Go has pointers (`&` to take an address, `*` to
> dereference) but **no pointer arithmetic** — you cannot do `dst + 1`. That
> removes an entire class of memory bugs.

### 2.8 `validate` — fail fast

```go
func (c *Config) validate() error {
	required := map[string]string{
		"mqtt.tls.cert_file":      c.MQTT.TLS.CertFile,
		"signing.key_file":        c.Signing.KeyFile,
		"license.issuer_pub_file": c.License.IssuerPubFile,
		// …
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
```

> **Go concept — `switch` with empty cases and no fallthrough.** Go's `switch`
> does **not** fall through by default (the opposite of C++), so `case
> "offline":` with an empty body simply matches-and-does-nothing. `default`
> catches every other value. This reads as a clean state machine: offline is
> fine; online needs a URL; anything else is an error. (Explicit `fallthrough`
> exists but is rarely used.)

This function encodes the fail-fast philosophy: a misconfiguration dies here,
at startup, with a message naming the exact key — never at 3 a.m. mid-request.

### 2.9 `tls.go` — building `*tls.Config`

```go
func ServerTLS(t TLS) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if t.ClientCAFile != "" {
		pool, err := loadPool(t.ClientCAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
```

> **Go concept — the standard library does the heavy lifting.** `crypto/tls` and
> `crypto/x509` ship with Go; there is no OpenSSL to link. `tls.LoadX509KeyPair`
> reads the PEM cert+key; `&tls.Config{…}` is a **struct literal with named
> fields** (you set only the ones you care about, the rest take zero values).
> `[]tls.Certificate{cert}` is a **slice literal** holding one element.

> **Go concept — optional behaviour via zero value.** Mutual TLS is switched on
> simply by whether `ClientCAFile` is non-empty. The empty string (its zero
> value) means "off". This is idiomatic Go: absence *is* the default, so you
> rarely need explicit `enabled bool` flags.

```go
func loadPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return pool, nil
}
```

An unexported helper (lowercase `loadPool`) shared by `ServerTLS` and
`ClientTLS` — DRY within the package, invisible outside it.

---

## 3. Deep dives

### 3.1 Implicit interfaces, felt for the first time

The single most important idea in this doc is that `Duration` became a
`yaml.Unmarshaler` **without declaring so**. Internally, when `yaml.Unmarshal`
walks the decoded tree and reaches a field of type `Duration`, it asks (via
reflection) "does this type have an `UnmarshalYAML` method?" — and because it
does, the library calls yours instead of its default. This is the mechanism
behind an enormous amount of Go: `fmt.Stringer` (custom printing),
`json.Marshaler`, `io.Reader`/`io.Writer` (doc 07), `error` itself. Learn to
ask "what interface does this library look for?" and you can hook into any of
them. The whole standard library is built on a handful of tiny interfaces.

### 3.2 Why a merge pipeline, and why validate last

Configuration in production comes from *layers*: sensible built-in defaults, a
file for per-deployment settings, and environment variables for
secrets/last-minute overrides (the "12-factor" model — the same binary runs
everywhere, behaviour changes only via config). `Load` composes these in
increasing priority and validates the *result*, so a value supplied by *any*
layer is checked. If validation ran before `applyEnv`, an env-supplied fix (or
an env-introduced mistake) would be missed.

---

## 4. Idioms & gotchas

- **`if err != nil` is the heartbeat of Go.** Expect it after almost every call.
  It is verbose on purpose: error paths are visible, not hidden in a `catch`.
- **Struct tags are unchecked strings.** `yaml:"cert_flie"` compiles fine and
  silently never binds. Tests (doc: `config_test.go`) guard against this.
- **Map iteration order is random.** `validate`'s `required` map may report a
  different missing field first on each run — fine here, but never depend on map
  order for logic or output.
- **Return `nil, err` on failure.** The convention is to return the zero value
  for every non-error result alongside the error; callers must not read those
  results when `err != nil`.
- **Unexported helpers keep the API small.** Only `Load`, `ServerTLS`,
  `ClientTLS`, `Config`, `TLS`, `Duration` are exported; `defaults`, `applyEnv`,
  `validate`, `loadPool` are private plumbing.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why must `UnmarshalYAML` use a pointer receiver while `Std` can
   use a value receiver? What would break if you swapped them?
2. **Apply.** Add a new setting `metrics.enabled bool` with a YAML tag and a
   default of `true`. Which functions must you touch, and why does the default
   need `defaults()` rather than relying on the zero value?
3. **Apply.** Add a `DEVLOG_S3_USE_TLS` override. Note the map-of-pointers
   trick only works for `*string`; how would you handle a `bool` field? (Hint:
   `strconv.ParseBool`.)
4. **Extend.** Make `validate` reject a `redis.hot_retention` shorter than
   `archive.flush_interval` (data would be trimmed before it's archived). Write
   a table-driven test for it (peek at doc: `config_test.go`).
5. **Hero.** Give `Config` a `String()` method that prints the config with
   secrets redacted. Which standard interface are you implementing, and how does
   `fmt.Println(cfg)` find it?

---

## 6. Recap & next

You now know how a Go package is shaped and how the language's foundations —
types, methods, interfaces, structs, tags, maps, pointers, and errors — combine
in real code. Every later component reuses all of this.

**Next:** [03 — telemetry](03-telemetry.md), where you meet function values,
`slog`, and Go's built-in HTTP server, and see dependency injection replace the
global variables you might reach for out of C++ habit.
