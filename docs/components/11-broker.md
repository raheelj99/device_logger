# Component 11 — `internal/broker`

> **Role:** embed the MQTT broker inside devlogd — TLS termination, license
> authentication, topic ACLs, and handoff of each publish into the ingest
> pipeline. | **Source:** `internal/broker/broker.go`, `internal/broker/hooks.go`

**Where this sits in the journey:** you have written logic; now you learn to
*plug into someone else's framework*. This is the first component built by
**embedding a third-party type** (`mqtt.HookBase`) and satisfying a large
interface by overriding only the few methods you care about. Prerequisites:
[02 — config](02-config.md) (structs, interfaces, TLS), [05 — license](05-license.md)
(the `Manager` you authorize against), [09 — ingest](09-ingest.md) (the
`Pipeline` you feed). New here: **embedding**, method sets, and adapter thinking.

## What you'll master

- **Go:** struct **embedding** and method **promotion**; satisfying an interface
  *partially* via an embedded base; **method sets**; the hook/callback pattern as
  middleware; wiring a third-party server library; `context.WithTimeout` +
  `defer cancel()`; string parsing (`strings.SplitN`, `strings.HasPrefix`);
  `bytes.Contains` over a byte set; returning `bool` from policy callbacks.
- **Domain:** an embedded MQTT broker, license-as-password authentication at
  CONNECT, a publish-only per-device topic ACL, and defense in depth.

---

## 1. Orientation

devlogd does not talk to an external MQTT broker — it *is* one. The
`mochi-mqtt` library is a broker you run **in-process**: you create a server,
attach *hooks* (callbacks it invokes at lifecycle points), add a TLS listener,
and call `Serve()`. Our two hooks turn a generic broker into devlogd's secured
front door:

- `authHook` — authenticates each CONNECT against a license and confines each
  session to its own topic namespace.
- `ingestHook` — hands every accepted publish to the ingest pipeline.

This is the *adapter* half of ports-and-adapters (you met the idea in
[07 — cold](07-cold.md)): the broker library is generic; our hooks adapt it to
the devlogd domain.

---

## 2. Guided code walkthrough

### 2.1 Constructing the server (`broker.go`)

```go
func New(listen string, tlsCfg *tls.Config, pipeline *ingest.Pipeline,
	sessions *license.Manager, metrics *telemetry.Metrics, log *slog.Logger) (*Broker, error) {
	srv := mqtt.New(&mqtt.Options{InlineClient: false, Logger: log.With("component", "mqtt")})
	if err := srv.AddHook(&authHook{sessions: sessions, metrics: metrics, log: log}, nil); err != nil {
		return nil, err
	}
	if err := srv.AddHook(&ingestHook{pipeline: pipeline, log: log}, nil); err != nil {
		return nil, err
	}
	if err := srv.AddListener(listeners.NewTCP(listeners.Config{
		ID: "mqtts", Address: listen, TLSConfig: tlsCfg,
	})); err != nil {
		return nil, err
	}
	return &Broker{srv: srv, log: log}, nil
}
```

> **Go concept — dependency injection at the constructor, again.** `New` takes
> everything it needs (`pipeline`, `sessions`, `metrics`, `log`) as parameters
> and stores them into the hook structs. Same pattern as every component: no
> globals, so the broker is independently testable. `log.With("component",
> "mqtt")` returns a *child logger* that stamps every line with that field — a
> `slog` idiom (see [03 — telemetry](03-telemetry.md)).

> **Go concept — wiring a third-party server.** `mqtt.New(...)` builds the
> library's server; `AddHook` registers our callbacks; `AddListener` +
> `listeners.NewTCP{TLSConfig: tlsCfg}` attaches a **TLS** listener (the
> `*tls.Config` we built in [02 — config](02-config.md), so MQTT is `mqtts://`).
> Each call returns an `error` we check immediately. Note the pattern: you don't
> subclass the server, you *compose* behaviour onto it via hooks.

### 2.2 `Run` — serve until cancelled

```go
func (b *Broker) Run(ctx context.Context) error {
	if err := b.srv.Serve(); err != nil {
		return err
	}
	<-ctx.Done()
	return b.srv.Close()
}
```

> **Go concept — the `Run(ctx)` lifecycle shape.** `Serve()` starts the broker's
> own goroutines and returns immediately; `<-ctx.Done()` then **blocks** until
> the context is cancelled (the receive from a context's done channel is the
> canonical "wait for shutdown" primitive — see [08 — archive](08-archive.md)),
> after which `Close()` shuts the broker down. Every long-lived component in
> devlogd exposes exactly this `Run(ctx) error` signature so the composition
> root ([14](14-composition-root.md)) can supervise them uniformly.

### 2.3 The hooks and embedding (`hooks.go`)

```go
type authHook struct {
	mqtt.HookBase
	sessions *license.Manager
	metrics  *telemetry.Metrics
	log      *slog.Logger
}

func (h *authHook) ID() string { return "license-auth" }

func (h *authHook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnConnectAuthenticate,
		mqtt.OnACLCheck,
		mqtt.OnSessionEstablished,
		mqtt.OnDisconnect,
	}, []byte{b})
}
```

> **Go concept — embedding (composition over inheritance).** Writing a type name
> with **no field name** — `mqtt.HookBase` — *embeds* it. All of `HookBase`'s
> methods are **promoted**: they become callable on `authHook` as if they were
> its own. `mochi-mqtt` requires hooks to implement a *large* `Hook` interface
> (dozens of `On…` methods). `HookBase` is a do-nothing implementation of every
> one of them; by embedding it, `authHook` **instantly satisfies the whole
> interface**, and you override only the handful of methods you care about. This
> is Go's answer to a C++ abstract base class with default virtual methods — but
> it is *composition*, not inheritance: there is no vtable, no `virtual`, and no
> "is-a" relationship, just a `HookBase` value living inside your struct whose
> methods are borrowed.

> **Go concept — method sets & interface satisfaction.** A type satisfies an
> interface when its **method set** includes every method the interface
> declares. Promoted (embedded) methods count toward the method set, which is
> exactly why embedding `HookBase` works. You never write `implements Hook`; the
> compiler checks the method set when you pass `&authHook{}` to `AddHook`.

> **Go concept — `byte`, byte-slice literals, and `bytes.Contains`.** `byte` is
> an alias for `uint8`. `mqtt.OnConnectAuthenticate` etc. are `byte` constants
> naming lifecycle events. `Provides(b byte) bool` tells the broker *which*
> events this hook wants; here it builds a `[]byte{…}` set of the four we handle
> and asks `bytes.Contains(set, []byte{b})` — "is `b` one of them?". Returning
> `false` for everything else means the broker skips this hook for those events
> (an opt-in dispatch, cheaper than calling every hook for every event).

### 2.4 Authentication at CONNECT

```go
func (h *authHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	device := string(pk.Connect.Username)
	token := string(pk.Connect.Password)
	if device == "" || token == "" {
		h.log.Warn("mqtt connect without credentials", "remote", cl.Net.Remote)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.sessions.Authorize(ctx, token, device, license.FeatureIngest); err != nil {
		h.log.Warn("mqtt session rejected", "device", device, "err", err)
		return false
	}
	h.log.Info("mqtt session authorized", "device", device)
	return true
}
```

> **Go concept — a `bool` policy callback.** The broker asks "may this client
> connect?" and the hook answers with a plain `bool`. `true` accepts, `false`
> rejects (the broker sends the failure CONNACK). No exceptions, no return code
> enums — the decision *is* the return value.

> **Go concept — `context.WithTimeout` + `defer cancel()`.** `Authorize` may do
> network I/O (online license mode). We create a **derived context** with a
> 10-second deadline from `context.Background()` (the root context, used when
> there is no parent to inherit from — an event callback has none). `defer
> cancel()` guarantees the context's resources are released the moment the
> function returns, whether or not the timeout fired. **Always** `defer cancel()`
> immediately after `WithTimeout`/`WithCancel`; forgetting it leaks a timer and
> a goroutine — the single most common context bug.

> **Go concept — `string(byteSlice)` conversion.** `pk.Connect.Username` is
> `[]byte` on the wire; `string(...)` copies it into an immutable Go string.
> This is the domain mapping that makes the module's licensing model concrete:
> **MQTT username = device id, MQTT password = the signed license token**, and
> the broker demands the `ingest` feature ([05 — license](05-license.md)).

### 2.5 The topic ACL — confinement

```go
func (h *authHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	if !write {
		return false
	}
	return strings.HasPrefix(topic, topicPrefix+string(cl.Properties.Username)+"/")
}
```

> **Go concept — string helpers & a tiny security policy.** `strings.HasPrefix`
> checks that the topic begins with `devlog/v1/<this-client's-username>/`. Two
> rules in four lines: (1) `if !write { return false }` — producers may
> **publish only**, never subscribe; (2) a client may only write under **its
> own** device prefix. So even a client with a valid license cannot publish as a
> *different* device. This is **defense in depth**: the ingest pipeline
> *independently* rechecks device identity ([09 — ingest](09-ingest.md)), so a
> bug in either layer alone cannot let a device forge another's logs.

### 2.6 Session accounting and the ingest handoff

```go
func (h *authHook) OnSessionEstablished(cl *mqtt.Client, _ packets.Packet) {
	h.metrics.MQTTConnections.Inc()
	h.sessions.SessionStarted(cl.ID)
}
func (h *authHook) OnDisconnect(cl *mqtt.Client, _ error, _ bool) {
	h.metrics.MQTTConnections.Dec()
	h.sessions.SessionEnded(cl.ID)
}
```

> **Go concept — the blank identifier `_` in parameters.** `OnDisconnect(cl,
> _, _)` accepts the error and "expected?" bool the interface demands but
> **discards** them with `_`. You must match the interface's signature exactly,
> but you needn't name arguments you ignore. The two methods keep the live
> connection gauge and license session table accurate.

```go
func (h *ingestHook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	device := string(cl.Properties.Username)
	subsystem := ""
	if parts := strings.SplitN(pk.TopicName, "/", 4); len(parts) == 4 {
		subsystem = parts[3]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.pipeline.Ingest(ctx, device, subsystem, pk.Payload); err != nil {
		h.log.Warn("ingest rejected", "device", device, "topic", pk.TopicName, "err", err)
	}
	return pk, nil
}
```

> **Go concept — `strings.SplitN` and defensive parsing.** The topic layout is
> `devlog/v1/{device}/{subsystem}`. `SplitN(s, "/", 4)` splits into **at most 4**
> parts (the last part keeps any remaining slashes), and we only read
> `parts[3]` when there are exactly four — never indexing a slice out of range
> (which would panic). `device` comes from the *authenticated* session
> (`cl.Properties.Username`), not the topic, so the identity is trustworthy.

> **Go concept — swallowing an error deliberately.** A failed `Ingest` is
> **logged but not returned as a fatal**; the method returns `pk, nil`, so the
> broker keeps the session alive. This is a considered choice: one malformed or
> rejected publish (bad payload, identity mismatch) must not disconnect an
> otherwise healthy producer. Returning a non-nil error here would tear down the
> connection. "Log and continue" is the right posture for per-message faults.

---

## 3. Deep dives

### 3.1 Embedding to satisfy a big interface — the idiom, and the trap

The reason `authHook` can be a valid `mqtt.Hook` while defining only six methods
is embedding + promotion. Picture the method set: `authHook` "has" every method
of `HookBase` (do-nothing defaults) *plus* the six it defines itself; when a
method name collides (e.g. `OnACLCheck`), **the outer type's method wins** (it
"shadows" the embedded one). So the broker calls your `OnACLCheck` and
`HookBase`'s version of everything you didn't override.

The trap: promotion is **name-based, not signature-checked against your
intent**. If you write `func (h *authHook) OnACLCheck(cl *mqtt.Client, topic
string, read bool) bool` with the wrong parameter *types*, you have not
overridden the interface method — you've added a *new, unrelated* method, and
`HookBase`'s default silently keeps running. The compiler won't warn you,
because the type still satisfies `Hook` (via the embedded default). Always copy
the exact signature from the library. (This is the embedding-equivalent of the
struct-tag-typo gotcha from [02 — config](02-config.md): silent, not a compile
error.)

### 3.2 Hooks as middleware, and the security boundary

Conceptually the two hooks are **middleware** around the broker's message flow —
the same idea you will see as gRPC interceptors in [12 — grpcapi](12-grpcapi.md),
just expressed through a library's callback interface instead of a function
chain. `authHook` is the *authentication + authorization* stage
(CONNECT-time license check, per-message ACL); `ingestHook` is the *handoff*
stage. Because the broker enforces `OnACLCheck` on **every** publish and
`OnConnectAuthenticate` on **every** connect, there is no code path into the
pipeline that skips them — security is structural, not something a new feature
can forget to call.

---

## 4. Idioms & gotchas

- **Embed a `…Base`/`…Default` type** to implement a large third-party
  interface cheaply; override only what you need.
- **Method promotion is name-based** — a signature typo creates a new method and
  silently leaves the default in place. No compile error.
- **`defer cancel()` right after `context.WithTimeout`.** Non-negotiable.
- **`context.Background()` for a root context** when a callback gives you no
  parent context to derive from.
- **Per-message failures: log and continue** — return `nil` so the transport
  stays up; only return an error for faults that should tear the connection down.
- **`SplitN` + a length check** avoids out-of-range panics on odd topics.
- **Trust the authenticated identity, not client-supplied data** — `device`
  comes from the session, not the topic string.

---

## 5. Exercises (zero → hero)

1. **Recall.** How does `authHook` satisfy the full `mqtt.Hook` interface while
   defining only six methods? What is being *promoted*?
2. **Recall.** Why does `OnPublish` return `pk, nil` even when `Ingest` fails?
   What would returning the error do?
3. **Apply.** Add an `OnSubscribe` policy that lets a session subscribe only to
   its own device prefix (mirroring the publish ACL). Which method must you
   override, and what happens if you misspell its signature?
4. **Apply.** Make `Provides` also handle a new hook event and add a metric for
   rejected publishes. Where does the counter get incremented?
5. **Hero.** The ACL rebuilds `topicPrefix + username + "/"` on every publish.
   Explain why this is safe against a client that sets a crafted `TopicName`,
   then write a test (embed `HookBase`, construct an `authHook`, call
   `OnACLCheck` with hostile topics) proving confinement.

---

## 6. Recap & next

You learned Go's headline composition mechanism — **embedding** — and used it to
satisfy a large third-party interface by overriding only what matters, the
essence of adapter/hook programming. You saw a broker turned into a secured
front door with `bool` policy callbacks, TLS wiring, timeouts, and a defense-in-
depth ACL.

**Next:** [12 — grpcapi](12-grpcapi.md), where the same middleware idea returns
as explicit **interceptor chains**, and you meet gRPC server streaming, context
metadata, and typed status codes.
