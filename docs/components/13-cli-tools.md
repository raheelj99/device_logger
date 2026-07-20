# Component 13 — the CLIs (`cmd/lictl`, `cmd/logctl`, `cmd/licensed`, `tools/gencerts`)

> **Role:** the operator- and operator-facing command-line tools — mint/inspect
> licenses (`lictl`), query/tail/verify/export and simulate a job (`logctl`), run
> the online license server (`licensed`), and generate a dev PKI (`gencerts`).
> | **Source:** `cmd/lictl/main.go`, `cmd/logctl/{main,grpc,sim}.go`,
> `cmd/licensed/main.go`, `tools/gencerts/main.go`

**Where this sits in the journey:** you have studied the *server*; now you build
the *clients and tools*, learning how a Go program becomes an executable and how
it talks to devlogd. Prerequisites: [02 — config](02-config.md) (TLS),
[04 — sign](04-sign.md) (keys), [05 — license](05-license.md) (licenses),
[12 — grpcapi](12-grpcapi.md) (the RPCs you now call as a client).

## What you'll master

- **Go:** `func main()`, `os.Args`, `os.Exit` codes, writing to `os.Stderr`; the
  **`flag`** package and `flag.FlagSet` **subcommands**; the subcommand `switch`
  dispatch; file I/O with unix perm bits (`os.WriteFile(..., 0o600)`,
  `os.MkdirAll`, `path/filepath.Join`); a gRPC **client** (`grpc.NewClient`,
  consuming a server stream with a `Recv()`/`io.EOF` loop); implementing
  **`PerRPCCredentials`** as a custom type that refuses plaintext; `protojson`.
- **Domain:** license issuance/inspection, operator observation, the online
  license server, dev certificate generation.

---

## 1. Orientation

Each directory under `cmd/` (and `tools/`) with a `package main` and a
`func main()` compiles to its **own binary** — this is how one Go module ships
several executables ([01 — proto contract](01-proto-contract.md) covered
modules). They share the `internal/` packages you've studied, so the tools are
thin: parse flags, call a package, print a result. Two patterns dominate:
**subcommand dispatch** (`lictl`, `logctl`) and a **gRPC client** (`logctl`'s
observation commands).

---

## 2. Guided code walkthrough

### 2.1 `main`, exit codes, and subcommand dispatch (`lictl`)

```go
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lictl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lictl <keygen|issue|inspect> [flags]")
	}
	switch args[0] {
	case "keygen":  return keygen(args[1:])
	case "issue":   return issue(args[1:])
	case "inspect": return inspect(args[1:])
	default:
		return fmt.Errorf("unknown command %q (want keygen, issue, or inspect)", args[0])
	}
}
```

> **Go concept — `func main` and `os.Args`.** Execution of a `package main`
> binary starts at `func main()` (no arguments, no return). `os.Args` is a
> `[]string` of the raw command line; `os.Args[0]` is the program name, so
> `os.Args[1:]` **slices off** the name to get the user's arguments. (Slicing a
> slice, from [10 — query](10-query.md), yields a view with no copy.)

> **Go concept — errors up, exit at the top.** The idiom is a tiny `main` that
> delegates to `run(...) error`, so all logic uses normal `return err` flow;
> `main` prints to **`os.Stderr`** (stderr, not stdout — errors don't pollute
> piped output) and sets the process exit code with `os.Exit(1)`. Non-zero =
> failure, the universal shell contract. Note: `os.Exit` runs **no deferred
> functions**, which is exactly why the real work lives in `run` (where `defer`
> still fires) and `main` only exits.

> **Go concept — subcommand dispatch with `switch`.** `switch args[0]` routes to
> a handler, passing the *remaining* args down. Go's `switch` needs no `break`
> (no fall-through — see [02 — config](02-config.md)); `default` catches unknown
> commands. This is the dependency-free way to build `git`-style CLIs.

### 2.2 Per-subcommand flags with `FlagSet` (`lictl issue`)

```go
func issue(args []string) error {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	keyFile     := fs.String("key", "deploy/keys/issuer.key", "issuer private key (PEM)")
	subject     := fs.String("subject", "", "device id this license is bound to, or * for any")
	features    := fs.String("features", "ingest,query", "comma-separated feature grants")
	days        := fs.Int("days", 365, "validity in days from now")
	maxSessions := fs.Int("max-sessions", 0, "concurrent device sessions (0 = unlimited)")
	out         := fs.String("out", "", "output .lic file (default <subject>.lic)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *subject == "" {
		return fmt.Errorf("-subject is required")
	}
	// … load issuer key, build license.License, license.Issue(), write token …
}
```

> **Go concept — the `flag` package.** Go's standard flag parser. `fs.String(
> name, default, usage)` returns a **`*string`** — a pointer whose pointee is
> filled in by `fs.Parse(args)`. You read the value later as `*subject`. Same for
> `fs.Int` (`*int`) and `fs.Duration` (`*time.Duration`). `flag.NewFlagSet` gives
> each subcommand its **own** flag namespace (so `lictl issue -days` and `lictl
> inspect -pub` don't collide); `ContinueOnError` makes `Parse` return an error
> instead of calling `os.Exit`, so `run` stays in control. This is the pointer
> version of the config-loading you saw in [02](02-config.md), for the CLI edge.

> **Go concept — reading flag values (deref) & validation.** `*subject` reads
> through the pointer; a required flag is enforced by checking the zero value
> (`""`) and returning an error — fail fast, like server config validation.

### 2.3 File I/O with permissions (`lictl keygen`, `gencerts`)

```go
if err := os.MkdirAll(*outDir, 0o700); err != nil { return err }
// …
if err := os.WriteFile(privPath, privPEM, 0o600); err != nil { return err }
if err := os.WriteFile(pubPath,  pubPEM,  0o644); err != nil { return err }
```

> **Go concept — unix perms as octal literals.** `0o600` is an **octal**
> integer literal (`rw-------`): owner read/write only — correct for a private
> key. `0o644` (`rw-r--r--`) is fine for a public key/cert; `0o700` for a
> directory. Go bakes the security decision into the write call. `os.WriteFile`
> creates-or-truncates in one call; `os.MkdirAll` makes the whole path (like
> `mkdir -p`). `path/filepath.Join(dir, name)` builds OS-correct paths — never
> concatenate with `"/"` by hand.

`gencerts` uses these plus `crypto/ecdsa`, `crypto/x509`, and
`crypto/x509/pkix` to create a CA and issue server/client certs with the right
SANs (`DNSNames`/`IPAddresses`) — the same PEM/DER machinery you met in
[04 — sign](04-sign.md), applied to X.509 instead of raw keys.

### 2.4 A gRPC client with a bearer credential (`logctl/grpc.go`)

```go
type bearerToken string

func (b bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearerToken) RequireTransportSecurity() bool { return true }

func dial(c *connFlags) (*grpc.ClientConn, devicelogv1.LogServiceClient, error) {
	tlsCfg, err := config.ClientTLS(*c.caFile)
	// … read token from *c.licFile …
	conn, err := grpc.NewClient(*c.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithPerRPCCredentials(bearerToken(strings.TrimSpace(string(token)))),
	)
	// …
	return conn, devicelogv1.NewLogServiceClient(conn), nil
}
```

> **Go concept — implementing an interface with a non-struct type.** `type
> bearerToken string` is a **defined type** ([02](02-config.md)) whose underlying
> type is `string`. Attaching `GetRequestMetadata` and `RequireTransportSecurity`
> to it makes it satisfy gRPC's `credentials.PerRPCCredentials` interface — a
> whole credential in two methods, no struct needed. The token *is* the value.

> **Go concept — a security invariant encoded in a method.**
> `RequireTransportSecurity() bool { return true }` tells gRPC this credential
> **must not** be sent over an insecure connection. If someone dialed without TLS,
> gRPC refuses to attach the token — the license can never travel in the clear.
> The safety property is enforced by the type, not by remembering to check.

> **Go concept — client construction.** `grpc.NewClient(addr, opts...)` mirrors
> the server's functional options: channel TLS via `WithTransportCredentials`,
> per-call auth via `WithPerRPCCredentials`. `NewLogServiceClient(conn)` returns
> the generated **client stub** — the mirror of the server interface from
> [12](12-grpcapi.md).

### 2.5 Consuming a server stream (`logctl query`)

```go
stream, err := client.Query(ctx, req)
if err != nil { return err }
n := 0
for {
	e, err := stream.Recv()
	if errors.Is(err, io.EOF) { break }
	if err != nil { return err }
	printEntry(e)
	n++
}
```

> **Go concept — the `Recv()`/`io.EOF` loop.** The client side of server
> streaming ([12](12-grpcapi.md)): call `Recv()` repeatedly; a normal end-of-
> stream is signaled by the sentinel error `io.EOF`, detected with
> `errors.Is(err, io.EOF)` (sentinel matching, from [06 — hot](06-hot.md)); any
> other error is real. `printEntry` uses `protojson.Marshal` to render the
> protobuf as JSON — the reverse of the encoding in [01](01-proto-contract.md).
> `context.WithTimeout` bounds the call; `defer conn.Close()` releases the
> connection.

### 2.6 An MQTT producer (`logctl sim`) and an HTTPS server (`licensed`)

`logctl sim` is the reference **producer**: it dials `ssl://…:8883` with the
paho MQTT client, sets username=device / password=license token, and publishes a
realistic sanitization job (`STARTED → PROGRESS → … → COMPLETED`) as serialized
`LogEntry` protobufs at QoS 1 — the exact contract the C++ app and the language
clients follow.

```go
opts := pahomqtt.NewClientOptions().
	AddBroker("ssl://" + *addr).SetUsername(*device).
	SetPassword(strings.TrimSpace(string(token))).SetTLSConfig(tlsCfg)
client := pahomqtt.NewClient(opts)
if t := client.Connect(); t.Wait() && t.Error() != nil { … }
defer client.Disconnect(250)
```

> **Go concept — the builder / fluent-options style.** paho uses chained setters
> returning the options object (`NewClientOptions().AddBroker(…).SetUsername(…)`)
> — another take on configurable construction. Its `Token` objects are futures:
> `t.Wait()` blocks, `t.Error()` reports the outcome.

`cmd/licensed` is a plain **HTTPS server**: it loads the issuer public key and
serves `license.NewServer(pub, log).Handler()` with `ListenAndServeTLS`, using
the same `signal.NotifyContext` + `srv.Shutdown` graceful-shutdown idiom you saw
in [03 — telemetry](03-telemetry.md) and will see assembled in
[14 — composition-root](14-composition-root.md).

---

## 3. Deep dives

### 3.1 The subcommand pattern without a framework

`lictl` and `logctl` prove you rarely need a third-party CLI library for a
tool: `switch` on `os.Args[1]`, hand the rest to a function that owns a
`flag.NewFlagSet(name, ContinueOnError)`. Benefits: each subcommand documents
its own flags, errors bubble to a single `main`, and there are zero
dependencies. The cost is you write the dispatch by hand — a few lines. For a
tool with dozens of commands you might reach for `cobra`, but the standard
library scales fine here and keeps the binary small and the code obvious.

### 3.2 Security invariants that types enforce

`bearerToken.RequireTransportSecurity()` is a small but important idea: instead
of *documenting* "don't send the token over plaintext" and hoping callers
comply, the credential's own method makes gRPC **refuse** to do so. Likewise
`0o600` on the private key file bakes the permission into the write. In both
cases the safe behavior is the *only* behavior the code permits — the same
philosophy as the server rejecting producer-set audit fields
([09 — ingest](09-ingest.md)). When you can, encode a security rule as a type or
a constant, not a comment.

---

## 4. Idioms & gotchas

- **Tiny `main`, real work in `run(...) error`** — because `os.Exit` skips
  `defer`s, and you want `defer` (Close, cancel) to run.
- **`flag.NewFlagSet(name, flag.ContinueOnError)` per subcommand** keeps you in
  control of exit behavior; `flag.String` returns a **pointer** you deref later.
- **Write secrets with `0o600`** and use `filepath.Join`, never string
  concatenation, for paths.
- **`errors.Is(err, io.EOF)`** ends a stream cleanly; treat any other error as a
  failure.
- **Errors and diagnostics go to `os.Stderr`**, results to stdout, so output
  stays pipeable.
- **Encode invariants in types** (`RequireTransportSecurity`) rather than relying
  on callers to remember.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why is the real logic in `run` rather than `main`? What does
   `os.Exit` skip?
2. **Recall.** How does `bearerToken` guarantee the license is never sent over an
   unencrypted connection?
3. **Apply.** Add a `lictl revoke` subcommand skeleton: wire the `switch` case
   and a `FlagSet` with a `-id` flag. What error do you return if `-id` is empty?
4. **Apply.** Change `logctl query` to also print a running count to stderr every
   100 entries. Which loop and which stream do you touch?
5. **Hero.** Implement `logctl stats` output as a table; then explain why the
   gRPC client must call `credentials.NewTLS` *and* `WithPerRPCCredentials` — what
   does each secure, and what fails if you drop either?

---

## 6. Recap & next

You can now build Go executables and tools: `main`/`run` structure, exit codes,
the `flag` package with subcommands, permissioned file I/O, a gRPC client that
consumes a server stream, and a credential type that structurally protects the
token. You have seen the producer (MQTT) and the license server from the client
side.

**Next:** [14 — composition-root](14-composition-root.md) — the capstone, where
`cmd/devlogd/main.go` wires every component you've studied together under
structured concurrency, and the whole system finally starts as one process.
