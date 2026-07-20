# Component 05 — `internal/license`

> **Role:** authenticate every session against an Ed25519-signed license —
> verified offline, optionally corroborated online against a license server that
> enforces session limits. | **Source:** `internal/license/license.go`,
> `internal/license/manager.go`, `internal/license/online.go`,
> `internal/license/server.go`

**Where this sits in the journey:** this is the package where Go's standard
library starts doing serious work for you. You already have the fundamentals —
structs, methods, interfaces, maps, pointers, and errors from
[02 — config](02-config.md); byte slices and `crypto/*` from
[04 — sign](04-sign.md). Here you assemble them into a real feature: a *credential*
system. New territory: `encoding/json`, the `time` package, `slices.Contains`,
your **first concurrency** (`sync.Mutex` around a shared map), and both sides of
`net/http` — the **client** that calls out and the **server** that answers.
Prerequisites: docs [02](02-config.md)–[04](04-sign.md).

## What you'll master

- **Go:** a `const (...)` block of string constants; `encoding/json`
  (`Marshal`/`Unmarshal`, `NewEncoder`/`NewDecoder`, `json:"…"` tags,
  deterministic field order); `encoding/base64`; the **`time` package**
  (`time.Time`, `time.Duration`, `time.Now()`, time comparisons, clock-skew
  arithmetic); `slices.Contains`; **`sync.Mutex`** guarding a shared `map`; the
  **`net/http` client** (`http.Client` with a timeout, `http.Transport` with a
  `TLSClientConfig`, `http.NewRequestWithContext`, `Do`, `defer
  resp.Body.Close()`); **`http.ServeMux`** with method+path patterns
  (`"POST /v1/activate"`); **nested maps** (`map[string]map[string]struct{}`) as a
  set-of-sets; and the **return-early-on-error** rhythm.
- **Domain:** the license *as* credential; ingest-vs-query feature separation;
  offline vs online modes; grace windows (availability over strictness); and
  server-side session limits.

---

## 1. Orientation

Every request into devlogd — an MQTT publish from a robot, a gRPC query from an
operator — must prove it is allowed. The proof is a **license**: a small record
(who it's for, what it may do, when it's valid, how many sessions) that the
issuer has **signed** with an Ed25519 private key. Nobody but the issuer can
forge one, and anybody with the issuer's *public* key can check one. That single
fact — *the license is the credential* — shapes the whole package. The signed
license, base64-encoded to one line, is literally the MQTT password and the gRPC
bearer token.

There are two verification modes. **Offline** (`Verifier`) needs only the public
key: check the signature, the validity window, the subject, the feature. It works
air-gapped — right for a robot in a field or a secure lab. **Online**
(`OnlineValidator`) adds a call to a central license server that can enforce
*global* limits like "at most 3 concurrent sessions", which no offline check can
know. `Manager` ties them together and is the single entry point both the MQTT
hook and the gRPC interceptors call.

---

## 2. Guided code walkthrough

### 2.1 The package doc comment and feature constants

```go
// Package license implements session authentication: Ed25519-signed license
// files verified offline, optionally corroborated online against a license
// server. The license itself is the credential — it is presented as the MQTT
// password and as the gRPC bearer token.
package license

const (
	FeatureIngest = "ingest"
	FeatureQuery  = "query"
)
```

> **Go concept — a `const` block of string constants.** `const ( … )` groups
> related compile-time constants (grouping is cosmetic, like a grouped `import`).
> These are **untyped string constants** — each behaves as a `string` wherever one
> is wanted. Being exported (capitalised), callers write `license.FeatureIngest`
> instead of the bare literal `"ingest"`, so a typo is a *compile* error at the
> call site, not a silently unmatched string. Go has no `enum class` of strings; a
> `const` block is the idiom.

These two constants are the **plane separation** in miniature: an `ingest`
license may write telemetry but not read it; a `query` license may read but not
write. One robot's station key cannot exfiltrate another's data.

### 2.2 The `License` struct and JSON tags

```go
type License struct {
	ID          string    `json:"lic_id"`
	Subject     string    `json:"subject"` // device id, or "*" for any
	Features    []string  `json:"features"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	MaxSessions int       `json:"max_sessions"` // 0 = unlimited
}
```

> **Go concept — `time.Time`.** `time.Time` is the standard library's instant-in-
> time type: an opaque struct (a wall clock reading *and* a monotonic reading)
> that you compare and format through its methods, never by poking at fields. It
> is value-typed — copying a `time.Time` copies the instant. `NotBefore` and
> `NotAfter` are the validity window; you'll see them compared in §2.9.

> **Go concept — struct tags for JSON.** You met struct tags with YAML in
> [doc 02](02-config.md). `` `json:"lic_id"` `` is the same mechanism for a
> different codec: `encoding/json` reads the tag by reflection to map the JSON key
> `lic_id` to the Go field `ID`. Only **exported** (capitalised) fields are
> (de)serialised — an unexported field is invisible to the encoder. The tag is an
> unchecked string, so `json:"lic_di"` compiles and silently never binds (the
> classic gotcha). Two extra tag options you'll meet below: `omitempty` (drop the
> field when it holds its zero value) and the fact that field *order* in the
> struct fixes the byte order of the output.

### 2.3 `Signed`, and why field order matters

```go
// Signed pairs a license with the issuer's Ed25519 signature over the
// license's canonical JSON bytes.
type Signed struct {
	License   License `json:"license"`
	Signature []byte  `json:"signature"`
}

// canonical returns the byte form that is signed. encoding/json emits struct
// fields in declaration order, which makes this deterministic.
func (l License) canonical() ([]byte, error) {
	return json.Marshal(l)
}
```

> **Go concept — `json.Marshal` and deterministic field order.** `json.Marshal(v)`
> turns any value into `[]byte` of JSON. For a **struct** it emits fields in
> *declaration order*, always — so `Marshal` of the same `License` yields the same
> bytes every run. That determinism is load-bearing here: the signature is
> computed over these exact bytes, so verification must reproduce them byte-for-
> byte. (Contrast a `map[string]T`, which `Marshal` sorts by key — also
> deterministic, but a different rule. Never assume a struct's JSON is "just the
> fields"; it is the fields *in order*.) The signing/verifying pair of
> deterministic marshaling was introduced in [doc 04](04-sign.md); this is the
> same discipline applied to JSON.

> **Go concept — `[]byte` in JSON.** `Signature []byte` marshals to a **base64
> string** automatically — `encoding/json` special-cases byte slices that way. So
> a `Signed` is entirely text-safe JSON even though the signature is raw bytes.

### 2.4 `Token` and `Parse` — base64 around JSON

```go
// Token renders the signed license as a single base64 line — the form used
// as MQTT password, gRPC bearer token, and .lic file content.
func (s *Signed) Token() (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func Parse(token string) (*Signed, error) {
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("license token is not valid base64: %w", err)
	}
	var s Signed
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("license token is not a signed license: %w", err)
	}
	return &s, nil
}
```

> **Go concept — `encoding/base64`.** `base64.StdEncoding` is a ready-made codec
> value; `.EncodeToString([]byte) string` and `.DecodeString(string) ([]byte,
> error)` are its two directions. Base64 exists to carry arbitrary bytes through
> text-only channels — an MQTT password field, an HTTP header, a `.lic` text file.
> The stack here is **JSON → base64**: `Token` marshals then encodes; `Parse`
> decodes then unmarshals, exactly reversing it.

> **Go concept — `json.Unmarshal` into a pointer.** `Unmarshal(raw, &s)` needs the
> **address** of the destination so it can fill it in — passing `s` by value would
> let it write into a throwaway copy. `var s Signed` first creates a zero-valued
> struct (recall zero values from [doc 02](02-config.md)); `Unmarshal` then
> populates only the fields present in the JSON, leaving the rest at zero. Unknown
> JSON keys are ignored by default.

> **Go concept — returning early on error.** Both functions show the Go heartbeat:
> after each fallible call, `if err != nil { return …, err }` — often wrapped with
> `%w`. There is no `try`/`catch`; the happy path stays at the left margin and
> every failure exits immediately. This "guard and return" shape repeats dozens of
> times in this package; train your eye to skim past it to the logic.

### 2.5 `HasFeature` — `slices.Contains`

```go
func (s *Signed) HasFeature(f string) bool {
	return slices.Contains(s.License.Features, f)
}
```

> **Go concept — `slices.Contains`.** `slices` (standard library, Go 1.21+) is a
> package of **generic** helpers over any `[]T`. `slices.Contains(haystack,
> needle)` reports whether the element is present — a linear scan, no
> boilerplate loop. It works on `[]string` here and on any comparable element type
> elsewhere; the compiler instantiates it per type. In C++ terms it is
> `std::find(...) != end` wrapped into a boolean, but without iterators. For a
> handful of features a linear scan is perfect; if `Features` held thousands you'd
> reach for a `map[string]struct{}` set instead (which you'll see next).

### 2.6 The `Verifier`

```go
// Verifier checks licenses offline against the issuer's public key.
type Verifier struct {
	pub  ed25519.PublicKey
	skew time.Duration
}

func NewVerifier(pub ed25519.PublicKey) *Verifier {
	return &Verifier{pub: pub, skew: 5 * time.Minute}
}
```

> **Go concept — `time.Duration` and duration literals.** `time.Duration` is an
> `int64` counting **nanoseconds** (you met it in [doc 02](02-config.md) as a
> config type). `5 * time.Minute` is arithmetic on that int64: `time.Minute` is a
> constant number of nanoseconds, multiplied by 5. The result prints as `5m0s`.
> `skew` is unexported and hard-wired here — a 5-minute tolerance for
> clock disagreement between issuer and verifier (§2.9). Storing it on the struct
> (rather than a package global) keeps `Verifier` self-contained and testable.

### 2.7 `Verify` — the offline decision

```go
// Verify checks the signature, validity window, subject binding, and feature
// grant. Empty subject/feature skip that specific check.
func (v *Verifier) Verify(s *Signed, subject, feature string, now time.Time) error {
	b, err := s.License.canonical()
	if err != nil {
		return err
	}
	if !ed25519.Verify(v.pub, b, s.Signature) {
		return fmt.Errorf("license %s: signature invalid", s.License.ID)
	}
	if now.Add(v.skew).Before(s.License.NotBefore) {
		return fmt.Errorf("license %s: not valid before %s", s.License.ID, s.License.NotBefore.Format(time.RFC3339))
	}
	if now.Add(-v.skew).After(s.License.NotAfter) {
		return fmt.Errorf("license %s: expired at %s", s.License.ID, s.License.NotAfter.Format(time.RFC3339))
	}
	if subject != "" && s.License.Subject != "*" && s.License.Subject != subject {
		return fmt.Errorf("license %s: issued to %q, presented by %q", s.License.ID, s.License.Subject, subject)
	}
	if feature != "" && !s.HasFeature(feature) {
		return fmt.Errorf("license %s: feature %q not granted", s.License.ID, feature)
	}
	return nil
}
```

This is the heart of the package; the full logic is unpacked in [§3.1](#31-offline-verification-the-license-is-the-credential).

> **Go concept — `time.Time` comparisons and skew arithmetic.** `time.Time`
> compares through **methods**, not operators: `a.Before(b)`, `a.After(b)`,
> `a.Equal(b)`. `now.Add(v.skew)` returns a *new* `time.Time` shifted forward by a
> duration (times are immutable — `Add` never mutates). So `now.Add(v.skew).Before(NotBefore)`
> means "even allowing our clock to be 5 minutes fast, we are still before the
> start" — a license only fails the not-yet-valid check when it is *clearly* too
> early. The symmetric `now.Add(-v.skew).After(NotAfter)` tolerates a 5-minute-
> slow clock on the expiry side. This ±skew band absorbs clock drift between the
> machine that issued the license and the one checking it.

> **Go concept — `now` as a parameter (testable time).** Notice `Verify` takes
> `now time.Time` rather than calling `time.Now()` itself. Injecting the clock
> lets a test pin "now" to any instant and check the exact window edges —
> impossible if the function read the real clock internally. The *caller*
> (`Manager`, §2.8) supplies `time.Now()`.

> **Go concept — `Format` with a reference layout.** `NotBefore.Format(time.RFC3339)`
> renders the time as a string. Go's format strings are unusual: instead of
> `%Y-%m-%d` you write an example of *the reference time* `Mon Jan 2 15:04:05 MST
> 2006`. `time.RFC3339` is a predefined layout constant for that standard.

### 2.8 `Manager` — one entry point, and the first mutex

```go
type Manager struct {
	offline *Verifier
	online  *OnlineValidator // nil in offline mode
	metrics *telemetry.Metrics
	log     *slog.Logger

	mu     sync.Mutex
	active map[string]struct{} // connection/session ids, for the gauge
}
```

`Manager.Authorize` is the single call both planes make:

```go
func (m *Manager) Authorize(ctx context.Context, token, subject, feature string) (*Signed, error) {
	s, err := Parse(token)
	if err != nil {
		return nil, err
	}
	if err := m.offline.Verify(s, subject, feature, time.Now()); err != nil {
		return nil, err
	}
	if m.online != nil {
		if err := m.online.Validate(ctx, s, subject); err != nil {
			return nil, err
		}
	}
	return s, nil
}
```

> **Go concept — a nil pointer as "feature off".** `online *OnlineValidator` is
> `nil` in offline mode. `if m.online != nil` is the whole switch: absence *is*
> the off state (the same zero-value idiom as optional mTLS in
> [doc 02](02-config.md)). Offline verification always runs; online is layered on
> only when configured — a strict superset, never a replacement.

Session tracking is where concurrency enters:

```go
func (m *Manager) SessionStarted(id string) {
	m.mu.Lock()
	m.active[id] = struct{}{}
	m.metrics.LicenseSessions.Set(float64(len(m.active)))
	m.mu.Unlock()
}

func (m *Manager) SessionEnded(id string) {
	m.mu.Lock()
	delete(m.active, id)
	m.metrics.LicenseSessions.Set(float64(len(m.active)))
	m.mu.Unlock()
}
```

> **Go concept — `sync.Mutex` guarding a shared map.** Many connections start and
> end **concurrently**, each on its own goroutine, all touching the one `active`
> map. Go maps are **not safe for concurrent write** — two goroutines writing at
> once is a data race that can corrupt the map or crash the program (the runtime
> may even detect it and panic). A `sync.Mutex` is a mutual-exclusion lock:
> `Lock()` blocks until this goroutine holds it exclusively, `Unlock()` releases
> it. Everything between is a **critical section** only one goroutine runs at a
> time — so the `map` write, the `len`, and the metric update happen atomically as
> a group. The mutex lives *next to the data it protects* (both fields of
> `Manager`), the standard Go layout; the convention is that `mu` guards the
> field(s) declared right after it. Unlike C++ there is no RAII `lock_guard` by
> default — you pair `Lock`/`Unlock` yourself (though `defer m.mu.Unlock()` is the
> common safety net; here the sections are tiny and straight-line, so explicit
> unlock reads fine).

> **Go concept — `struct{}` and the set idiom.** `map[string]struct{}` is a **set**:
> the keys are what matter, and `struct{}{}` is the **empty struct** — a value that
> occupies *zero bytes*. `m.active[id] = struct{}{}` records membership using no
> storage for the value. `delete(map, key)` removes an entry (a no-op if absent).
> `len(m.active)` is the live session count driving the gauge. In C++ you'd reach
> for `std::unordered_set`; Go has no set type, so "map to empty struct" is the
> canonical stand-in.

### 2.9 `OnlineValidator` — the HTTP client

```go
type OnlineValidator struct {
	url   string
	hc    *http.Client
	grace time.Duration
	log   *slog.Logger

	mu     sync.Mutex
	lastOK map[string]time.Time // license ID → last successful activation
}

func NewOnlineValidator(url, caFile string, grace time.Duration, log *slog.Logger) (*OnlineValidator, error) {
	tlsCfg, err := config.ClientTLS(caFile)
	if err != nil {
		return nil, err
	}
	return &OnlineValidator{
		url:    url,
		hc:     &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
		grace:  grace,
		log:    log,
		lastOK: map[string]time.Time{},
	}, nil
}
```

> **Go concept — `http.Client` with a timeout.** You saw the HTTP *server* in
> [doc 03](03-telemetry.md); this is the *client*. `&http.Client{Timeout: 10 *
> time.Second}` builds a reusable client whose `Timeout` caps the **entire**
> request (connect + write + read). Never use `http.DefaultClient` for outbound
> calls to another service — its timeout is *zero*, meaning **infinite**, so one
> hung server would pin a goroutine forever. An `http.Client` is safe for
> concurrent use, so one instance is shared across all validations.

> **Go concept — `http.Transport` and `TLSClientConfig`.** The `Transport` is the
> layer that actually opens connections and pools them. Setting
> `&http.Transport{TLSClientConfig: tlsCfg}` tells it which CA to trust for HTTPS —
> the same `*tls.Config` machinery from [doc 02](02-config.md), reused for the
> client side via `config.ClientTLS`. This is how the validator verifies it is
> really talking to the license server and not an impostor.

Now the request itself:

```go
func (o *OnlineValidator) Validate(ctx context.Context, s *Signed, deviceID string) error {
	token, err := s.Token()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(ActivateRequest{Token: token, DeviceID: deviceID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/v1/activate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.hc.Do(req)
	if err != nil {
		return o.graceOrDeny(s.License.ID, err)
	}
	defer resp.Body.Close()

	var ar ActivateResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return o.graceOrDeny(s.License.ID, fmt.Errorf("bad license server response: %w", err))
	}
	if !ar.OK {
		return fmt.Errorf("license server rejected session: %s", ar.Reason)
	}
	o.mu.Lock()
	o.lastOK[s.License.ID] = time.Now()
	o.mu.Unlock()
	return nil
}
```

> **Go concept — `http.NewRequestWithContext` and `Do`.** Build a request with
> `NewRequestWithContext(ctx, method, url, body)`; send it with `client.Do(req)`.
> Threading `ctx` through means that when the caller's context is cancelled (the
> connection dropped, or a deadline hit), the in-flight HTTP call is aborted too —
> no orphaned request outliving the thing that wanted it. `http.MethodPost` is the
> constant `"POST"`; the body is any `io.Reader`, here a `bytes.NewReader` over the
> marshaled JSON.

> **Go concept — `defer resp.Body.Close()`.** `Do` returns a response whose `Body`
> is an open stream backed by a live network connection. You **must** close it or
> the connection leaks and can never be reused from the pool. `defer` schedules the
> close to run when the function returns, no matter which `return` path is taken —
> the idiomatic way to guarantee cleanup (much like an RAII destructor, but you
> write it explicitly at the point of acquisition). Note it comes *after* the
> `err != nil` check: on error `resp` may be `nil`, so you only defer the close
> once you know the response exists.

> **Go concept — `json.NewDecoder(r).Decode(v)` vs `Unmarshal`.** When the JSON is
> a **stream** (an HTTP body), `json.NewDecoder(resp.Body).Decode(&ar)` reads and
> parses straight from the reader — no need to buffer the whole body into `[]byte`
> first. Use `Unmarshal` when you already hold the bytes (as in `Parse`); use a
> `Decoder` when you have a reader. The mirror on the write side is
> `json.NewEncoder(w).Encode(v)` (§2.11).

Note the two-tier failure handling: a **transport** error (`Do` failed — server
unreachable) routes to `graceOrDeny`, whereas a **definitive** rejection
(`!ar.OK`) is returned immediately. That distinction is the whole point of the
next section.

### 2.10 `graceOrDeny` — availability over strictness

```go
func (o *OnlineValidator) graceOrDeny(licID string, cause error) error {
	o.mu.Lock()
	last, seen := o.lastOK[licID]
	o.mu.Unlock()
	if seen && time.Since(last) <= o.grace {
		o.log.Warn("license server unreachable, session allowed under grace",
			"license", licID, "last_activation", last, "err", cause)
		return nil
	}
	return fmt.Errorf("license server unreachable and no grace available: %w", cause)
}
```

> **Go concept — `time.Since` and the comma-ok map read.** `last, seen :=
> o.lastOK[licID]` uses the **comma-ok** form of a map lookup: `last` is the value
> (or the zero `time.Time` if absent) and `seen` is a bool saying whether the key
> existed — the only reliable way to tell "never activated" from "activated at the
> zero time". `time.Since(last)` is shorthand for `time.Now().Sub(last)`, a
> `time.Duration`; comparing it `<= o.grace` asks "was the last success recent
> enough?" The grace policy is unpacked in [§3.2](#32-online-validation-grace-and-session-limits).

### 2.11 `Server` — the license server (`cmd/licensed`)

```go
type Server struct {
	ver *Verifier
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]map[string]struct{} // license ID → device ids
}
```

> **Go concept — nested maps as a set-of-sets.** `map[string]map[string]struct{}`
> maps a **license ID** to a **set of device IDs** (the inner `map[...]struct{}`
> set from §2.8). So the outer map answers "which devices are active under this
> license?" and its `len` is that license's live session count — exactly what
> `max_sessions` is measured against. A nested map's inner map is `nil` until you
> create it, so you must allocate it before first use (see `activate` below);
> writing to a nil map **panics**.

The routing table:

```go
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/activate", s.activate)
	mux.HandleFunc("POST /v1/heartbeat", s.activate) // idempotent re-activation
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
```

> **Go concept — `http.ServeMux` with method+path patterns.** A `ServeMux` is a
> request router. Since Go 1.22 a pattern may include the **method**: `"POST
> /v1/activate"` matches only POSTs to that path — a GET gets an automatic `405
> Method Not Allowed`, no manual `if r.Method != …` check. `HandleFunc` registers
> a function of signature `func(http.ResponseWriter, *http.Request)` (the handler
> shape from [doc 03](03-telemetry.md)). Both `activate` and `heartbeat` point at
> the *same* handler because re-activating an already-known device is idempotent —
> a heartbeat is just an activation that (usually) changes nothing. `Handler()`
> returns the mux as an `http.Handler` interface, hiding the concrete type.

The handler enforcing the limit:

```go
func (s *Server) activate(w http.ResponseWriter, r *http.Request) {
	var req ActivateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, http.StatusBadRequest, ActivateResponse{Reason: "malformed request"})
		return
	}
	signed, err := Parse(req.Token)
	if err != nil {
		respond(w, http.StatusForbidden, ActivateResponse{Reason: err.Error()})
		return
	}
	if err := s.ver.Verify(signed, req.DeviceID, "", time.Now()); err != nil {
		s.log.Warn("activation rejected", "device", req.DeviceID, "err", err)
		respond(w, http.StatusForbidden, ActivateResponse{Reason: err.Error()})
		return
	}

	lic := signed.License
	s.mu.Lock()
	devices, ok := s.sessions[lic.ID]
	if !ok {
		devices = map[string]struct{}{}
		s.sessions[lic.ID] = devices
	}
	_, known := devices[req.DeviceID]
	if !known && lic.MaxSessions > 0 && len(devices) >= lic.MaxSessions {
		s.mu.Unlock()
		s.log.Warn("activation rejected: session limit", "license", lic.ID, "device", req.DeviceID)
		respond(w, http.StatusForbidden, ActivateResponse{Reason: "max sessions exceeded"})
		return
	}
	devices[req.DeviceID] = struct{}{}
	s.mu.Unlock()

	s.log.Info("session activated", "license", lic.ID, "device", req.DeviceID)
	respond(w, http.StatusOK, ActivateResponse{OK: true, ExpiresAt: lic.NotAfter})
}
```

> **Go concept — manual `Unlock` on every exit path.** This critical section is
> longer and has *two* exits: the limit-exceeded branch and the success branch.
> Each calls `s.mu.Unlock()` before returning. Because there is no single exit,
> the code unlocks explicitly on each — miss one and every later request would
> deadlock waiting for a lock that is never released. (`defer s.mu.Unlock()` would
> avoid that risk but would also hold the lock across the logging and `respond`
> I/O, needlessly widening the critical section — a deliberate trade-off.)

```go
func respond(w http.ResponseWriter, status int, body ActivateResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
```

> **Go concept — `json.NewEncoder(w).Encode` and header order.** The mirror of the
> decoder: `Encode` marshals `body` straight to the `ResponseWriter` stream. Order
> matters — set headers, then `WriteHeader(status)` to send the status line, then
> write the body; once the body starts the status is locked in. The `_ =` discards
> the encoder's error deliberately: the response is already committed, so there is
> nothing useful to do with a late write failure, and `_ =` documents "I chose to
> ignore this" (an unassigned error would be flagged by linters).

`ActivateRequest`/`ActivateResponse` (in `online.go`) are the shared wire types,
so client and server agree on the JSON by construction:

```go
type ActivateResponse struct {
	OK        bool      `json:"ok"`
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}
```

> **Go concept — `omitempty`.** With `json:"reason,omitempty"` the field is omitted
> from the output entirely when it holds its zero value (`""`, here). A successful
> response thus carries no `reason` key at all, keeping the JSON tight.

---

## 3. Deep dives

### 3.1 Offline verification: the license *is* the credential

`Verify` runs four independent checks, each a guard that returns early on
failure, so the function reaches `return nil` only when *all* pass:

1. **Signature.** `ed25519.Verify(pub, canonical, sig)` recomputes the canonical
   JSON and checks it against the signature with the issuer's public key. If even
   one byte of the license was altered, or it was signed by the wrong key, this
   fails. This is why deterministic marshaling (§2.3) is non-negotiable: verifier
   and issuer must produce *identical* bytes.
2. **Validity window**, with ±`skew`. The license carries `NotBefore`/`NotAfter`;
   the ±5-minute band tolerates clock disagreement between machines.
3. **Subject binding.** The license names a `Subject` (a device id, or `*` for
   any). If a concrete subject is presented, it must match — so a license issued
   to `robot-7` cannot be replayed by `robot-9`.
4. **Feature grant.** `ingest` vs `query` — the plane separation from §2.1.

The profound part is what is **absent**: there is no user database, no session
table, no network call. Everything needed to authorize is *inside the signed
license*, and the only secret required to check it is the issuer's **public**
key. That is what "the license is the credential" means — like a signed passport,
it is self-verifying. The empty-string escape hatches (`subject == ""` or
`feature == ""` skip that check) let the same function serve callers who only
care about *some* of the checks — the license server, for instance, verifies
signature + window + subject but passes `""` for feature because it does not care
which plane a session is for.

### 3.2 Online validation: grace and session limits

Offline verification can prove a license is *authentic and unexpired*, but it
cannot know a *global* fact like "this license already has 3 active sessions
elsewhere in the fleet". That requires a central authority — the `Server`. The
`OnlineValidator` POSTs each activation to it; the server keeps the nested
`sessions` map and rejects a *new* device once `len(devices) >= MaxSessions`.
Note the `!known` guard: a device already counted can re-activate freely (that's
what makes `/v1/heartbeat` idempotent), so a network blip that triggers a re-POST
never bumps a device out of its own slot.

The subtle engineering is `graceOrDeny`. Two failure kinds are treated
oppositely on purpose:

- A **definitive rejection** — the server answered `ok: false` (bad signature,
  limit exceeded) — is final. Deny immediately; the answer is authoritative.
- A **transport failure** — the server is *unreachable* — is ambiguous. Maybe the
  license is fine and the network merely blipped. Denying here would take a
  perfectly licensed field robot offline because a *server we run* had a bad
  minute. So any license that activated successfully within the last `grace`
  window (default 72h) is allowed through, with a `Warn` log for the audit trail.

This is a deliberate **availability-over-strictness** trade-off, valid precisely
because offline verification already ran first: an attacker can't exploit the
grace path without a genuinely signed, unexpired, correctly-bound license — the
grace only relaxes the *global session-count* check, never the cryptographic
ones. Security in depth: the strict layer is the crypto; the lenient layer is the
availability policy on top of it.

---

## 4. Idioms & gotchas

- **Guard, return, repeat.** Offline `Verify`, `Authorize`, and `activate` are all
  a stack of `if bad { return err }` checks ending in success. Read the checks;
  the last line is the happy path.
- **A mutex guards the field(s) right after it.** `mu sync.Mutex` sits directly
  above `active` / `lastOK` / `sessions`. That placement *is* the documentation of
  what the lock protects. Every access to those maps takes the lock.
- **Nil inner maps panic.** In the nested `sessions` map you must create the inner
  `map[string]struct{}` (the `if !ok { … }` block) before writing to it. A read of
  a missing key is safe (comma-ok), but a *write* to a nil map panics.
- **Close every response body.** `defer resp.Body.Close()` after a successful
  `Do`, always — and only after the `err != nil` check, since `resp` is nil on
  error.
- **Never `http.DefaultClient` for service calls.** Its zero timeout is infinite.
  Construct an `http.Client` with an explicit `Timeout`.
- **Deterministic JSON is a contract, not a convenience.** The signature depends
  on `Marshal` producing identical bytes. Reordering the `License` fields, or
  switching to a `map`, would silently invalidate every issued license.
- **`Decode` for streams, `Unmarshal` for bytes; `Encode`/`Marshal` mirror them.**
  Pick by whether you hold a reader or a `[]byte`.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why does `Verify` take `now time.Time` as a parameter instead of
   calling `time.Now()` itself? What does that buy the tests?
2. **Recall.** What is `struct{}{}` and why is `map[string]struct{}` the right type
   for `active`? What would `map[string]bool` cost that this avoids?
3. **Apply.** Add a `FeatureAdmin = "admin"` constant and make `Authorize` accept
   it. Which existing functions already handle a new feature string with no change,
   and why?
4. **Apply.** `Validate` marshals its request with `body, _ := json.Marshal(...)`,
   discarding the error. Is that safe here? Under what input could `Marshal` of an
   `ActivateRequest` actually fail? (Hint: consider the field types.)
5. **Extend.** The `Server` never *removes* sessions — a device counts forever.
   Add a `POST /v1/deactivate` endpoint that deletes a device from its license's
   set, and make the inner set drop out of `sessions` when it becomes empty. What
   must you hold while doing it?
6. **Hero.** The grace window trusts the client's `lastOK` clock. Design a
   tightening that bounds total offline time even across process restarts (when
   the in-memory `lastOK` map is lost). What new state, and where, and what does it
   cost in availability?

---

## 6. Recap & next

You have built a real credential system and, along the way, met the workhorses of
day-to-day Go: `encoding/json` in all four forms (`Marshal`/`Unmarshal` for bytes,
`Encoder`/`Decoder` for streams), `encoding/base64`, the `time` package and its
skew-tolerant comparisons, `slices.Contains`, both ends of `net/http`, and — most
importantly — your first `sync.Mutex` guarding shared maps against concurrent
access. That last idea, the critical section, is the gateway to everything
concurrent that follows.

**Next:** [06 — hot](06-hot.md), where you meet the Redis client, pipelines and
transactions, `errors.Is` with sentinel errors, and callback-driven APIs — the
hot storage tier that every authorized `ingest` session writes into.
