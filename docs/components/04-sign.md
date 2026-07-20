# Component 04 — `internal/sign`

> **Role:** the tamper-evidence core — hash each log entry, sign it with
> Ed25519, chain it to the device's previous entry, and load/marshal the keys.
> | **Source:** `internal/sign/sign.go`, `internal/sign/keys.go`

**Where this sits in the journey:** this is your first deep dive into Go's
`crypto/*` standard library and into the language's most C++-surprising type
distinction — **arrays vs slices**. Prerequisites: [02 — config](02-config.md)
(structs, methods, receivers, interfaces, maps, pointers, errors) and
[03 — telemetry](03-telemetry.md) (function values, dependency injection). This
doc assumes those and goes deep on what is new: `[]byte`, `crypto/ed25519`,
`crypto/sha256`, `crypto/rand`, `crypto/x509` + `encoding/pem`, type assertions,
and small focused interfaces.

## What you'll master

- **Go:** `[]byte` byte slices and the `[]byte("…")` / `string(b)` conversions;
  **arrays vs slices** (a fixed `[32]byte` vs a `[]byte`, and the `h[:]`
  slice-of-array trick); the `crypto/*` packages (`ed25519`, `sha256`, `rand`);
  **type assertions** (`key.(ed25519.PrivateKey)` and the comma-ok form);
  `crypto/x509` + `encoding/pem` (DER vs PEM, PKCS8/PKIX); **small focused
  interfaces** (a `Signer`/`Verifier` as a method set); `proto.Clone` and
  `proto.MarshalOptions{Deterministic: true}`; `bytes.Equal`; a
  `map[string]ed25519.PublicKey` for key-ID lookup; **value vs pointer
  semantics** for keys.
- **Domain:** SHA-256 hashing, Ed25519 signatures, per-device hash chaining, and
  the audit block that turns a log into tamper *evidence*.

---

## 1. Orientation

A device log is only trustworthy if you can prove it was not altered after the
fact. `internal/sign` provides that proof. For every `LogEntry` it computes a
**canonical SHA-256 hash**, signs that hash with an **Ed25519** private key, and
records both — plus a link to the *previous* entry's hash — in the entry's
`Audit` block. The result is a per-device **hash chain**: change one byte of one
entry and its hash no longer matches; delete or reorder entries and the
`prev_hash` links no longer line up. Any tampering is detectable at a *provable*
point.

The package has two halves. `keys.go` handles key material: generating pairs,
and converting between in-memory keys and on-disk PEM files. `sign.go` handles
the runtime: the `Signer` and `Verifier` types, and the three functions that do
the work — `EntryHash`, `ChainSign`, `VerifyEntry`.

---

## 2. Guided code walkthrough

### 2.1 The package clause and imports

```go
// Package sign provides Ed25519 signing, per-device hash chaining, and PEM
// key handling — the tamper-evidence core of the audit trail.
package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)
```

> **Go concept — the `crypto/*` standard library.** Go ships production-grade
> cryptography *in the standard library* — no OpenSSL to link, no third-party
> dependency to vet. `crypto/ed25519` is the signature scheme, `crypto/sha256`
> the hash, `crypto/rand` a **cryptographically secure** random source, and
> `crypto/x509`/`encoding/pem` the key-serialization formats. In C++ you would
> reach for a library and manage its lifetime; here it is all `import` lines that
> `go.mod` already pins. Note `crypto/rand` (secure) is a *different package*
> from `math/rand` (fast, predictable, never for keys) — the import path is your
> only guard, so read it carefully.

### 2.2 Generating a key pair — `[]byte` under the hood

```go
func GenerateKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}
```

> **Go concept — `[]byte`, the byte slice.** `ed25519.PublicKey` and
> `ed25519.PrivateKey` are both **defined types whose underlying type is
> `[]byte`** — a slice of bytes. A byte slice is Go's universal "blob of raw
> bytes": a length, a capacity, and a pointer to a backing array. It is what you
> pass to every hash, signature, and I/O call. Because these key types are *just*
> `[]byte` with a name (the defined-type idea from doc 02), a public key is
> literally the 32 raw key bytes, and a signature is a 64-byte `[]byte`. There is
> no opaque handle to free — the garbage collector owns it.

> **Go concept — `crypto/rand` and `rand.Reader`.** `rand.Reader` is an
> `io.Reader` (the streaming interface you meet in doc 07) backed by the OS
> entropy source. `ed25519.GenerateKey` reads the randomness it needs from it.
> Passing the reader in — rather than the function reaching for a global — is
> dependency injection again (doc 03): a test can substitute a deterministic
> reader. This function is a one-line pass-through, so it simply forwards all
> three return values of `GenerateKey` straight out.

### 2.3 Value vs pointer semantics for keys

Notice these functions take and return `ed25519.PrivateKey` **by value**, not
`*ed25519.PrivateKey`.

> **Go concept — value semantics for slice-backed types.** A `[]byte` (and thus a
> key) is a small **header** — pointer, length, capacity — even when the data it
> points at is large. Copying the header is cheap and *both copies share the same
> backing array*. So passing a key "by value" does not copy the key bytes; it
> copies a 24-byte header that still refers to them. This is why crypto keys are
> passed as plain values in Go, not pointers — contrast a C++ `std::vector<byte>`
> where a by-value pass deep-copies. The gotcha: because copies share storage,
> one holder mutating the bytes is visible to the other. Keys are treated as
> read-only, so it never bites — but never assume a `[]byte` copy is independent.

### 2.4 Marshaling keys — DER, PEM, PKCS8, PKIX

```go
func MarshalPrivatePEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func MarshalPublicPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	// … same shape: DER → PEM block "PUBLIC KEY" …
}
```

> **Go concept — DER vs PEM, and the two-step marshal.** Serializing a key is two
> stages. First `crypto/x509` encodes it to **DER** — a compact binary format.
> **PKCS8** is the standard container for a *private* key of any algorithm;
> **PKIX** (`SubjectPublicKeyInfo`) is the standard container for a *public* key.
> Then `encoding/pem` wraps the DER in **PEM** — the Base64 text with
> `-----BEGIN PRIVATE KEY-----` banners you have seen in `.pem` files. DER is
> what tools parse; PEM is what humans and config files store. `pem.Block` is a
> struct with a `Type` (the banner label) and `Bytes` (the DER payload); we build
> one as a literal and hand its **address** (`&pem.Block{…}`) to
> `EncodeToMemory`, which returns the `[]byte` text.

### 2.5 Loading keys — `pem.Decode` and type assertions

```go
func LoadPrivatePEM(path string) (ed25519.PrivateKey, error) {
	block, err := readPEM(path, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key %s: %w", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 private key", path)
	}
	return priv, nil
}
```

> **Go concept — type assertions, comma-ok form.** `x509.ParsePKCS8PrivateKey`
> returns `any` (Go's `interface{}`, the empty interface that holds *any* value)
> — because a PKCS8 file could contain an RSA, ECDSA, or Ed25519 key. To get back
> a concrete type you write a **type assertion**: `key.(ed25519.PrivateKey)` says
> "I claim the dynamic value inside this interface is an `ed25519.PrivateKey`".
> The **comma-ok** form `priv, ok := key.(…)` is the safe one: `ok` is `false`
> (and `priv` is the zero value) if the claim is wrong, so we return a precise
> error instead of crashing. The *single-value* form `key.(T)` would **panic** on
> a mismatch — avoid it unless you are certain. This is the runtime equivalent of
> a C++ `dynamic_cast`: the comma-ok form is `dynamic_cast<T*>` returning
> `nullptr`; the bare form is `dynamic_cast<T&>` throwing `std::bad_cast`.

```go
func readPEM(path, wantType string) (*pem.Block, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != wantType {
		return nil, fmt.Errorf("%s: expected a %s PEM block", path, wantType)
	}
	return block, nil
}
```

> **Go concept — the blank identifier `_`.** `pem.Decode` returns
> `(*pem.Block, []byte)` — the first block, and the *rest* of the input after it.
> We do not care about the trailing bytes, so we discard them with `_`, the
> **blank identifier**. Assigning to `_` satisfies Go's "no unused variables"
> rule while signalling "intentionally ignored". The subsequent
> `block == nil || block.Type != wantType` guards against a file that was not PEM
> at all *and* against a file that is the wrong *kind* of PEM (a public key where
> a private one was expected) — both are rejected with the same clear message.

`LoadPublicPEM` mirrors this exactly with `ParsePKIXPublicKey` and the
`ed25519.PublicKey` assertion.

### 2.6 The `Signer` — a tiny stateful type

```go
type Signer struct {
	priv  ed25519.PrivateKey
	keyID string
}

func NewSigner(priv ed25519.PrivateKey, keyID string) *Signer {
	return &Signer{priv: priv, keyID: keyID}
}

func LoadSigner(keyFile, keyID string) (*Signer, error) {
	priv, err := LoadPrivatePEM(keyFile)
	if err != nil {
		return nil, fmt.Errorf("load signing key: %w", err)
	}
	return &Signer{priv: priv, keyID: keyID}, nil
}

func (s *Signer) KeyID() string             { return s.keyID }
func (s *Signer) Public() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }
func (s *Signer) Sign(digest []byte) []byte { return ed25519.Sign(s.priv, digest) }
```

> **Go concept — small focused interfaces (method sets).** `Signer` bundles a
> private key with the **key ID** that names it, and exposes exactly three
> methods. Those methods — `KeyID`, `Public`, `Sign` — form a **method set**.
> Nothing here declares "implements X"; but any consumer that needs "something I
> can call `Sign(digest) []byte` on" can define its own one-method interface and
> `*Signer` satisfies it implicitly (the doc-02 rule). Go culture favours these
> tiny interfaces — often a single method — declared *at the consumer*, not a
> broad `ICrypto` base class declared at the producer. Keep types small and let
> composition, not inheritance, assemble them.

> **Go concept — a type assertion inside `Public`.** `s.priv.Public()` returns
> `crypto.PublicKey`, which is an alias for `any`. We *know* an Ed25519 private
> key yields an Ed25519 public key, so the bare assertion `.(ed25519.PublicKey)`
> is used here — a case where panicking on the "impossible" is acceptable because
> a failure would signal a broken standard library, not bad user input.

### 2.7 The `Verifier` — a map for key rotation

```go
type Verifier struct {
	keys map[string]ed25519.PublicKey
}

func NewVerifier() *Verifier {
	return &Verifier{keys: map[string]ed25519.PublicKey{}}
}

func (v *Verifier) Add(keyID string, pub ed25519.PublicKey) {
	v.keys[keyID] = pub
}

func (v *Verifier) Verify(keyID string, digest, sig []byte) error {
	pub, ok := v.keys[keyID]
	if !ok {
		return fmt.Errorf("unknown signing key %q", keyID)
	}
	if !ed25519.Verify(pub, digest, sig) {
		return fmt.Errorf("signature invalid for key %q", keyID)
	}
	return nil
}
```

> **Go concept — `map[string]ed25519.PublicKey` and comma-ok lookup.** The
> `Verifier` is a hash map from **key ID** to **public key**. `NewVerifier`
> initialises it to an empty (non-nil) map with `map[…]…{}` — writing to a `nil`
> map panics, so the constructor exists precisely to hand you a ready one.
> `pub, ok := v.keys[keyID]` is the **comma-ok map lookup**: `ok` distinguishes
> "key ID is registered" from "absent", so an unknown ID becomes a clean error
> rather than a silent zero-value key that would then fail verification with a
> confusing message. This is the same comma-ok shape you saw for env vars in
> doc 02 and type assertions above — one idiom, three uses.

> **Go concept — `ed25519.Verify` returns a `bool`, not an error.** Verification
> is a pure predicate: signature valid or not. `ed25519.Verify(pub, digest, sig)`
> returns `true`/`false`, and we translate `false` into a descriptive `error` at
> our layer. This is deliberate API design — the crypto primitive answers the
> math question; the *application* decides what a failure *means* and phrases it.

Storing keys by ID is what makes **key rotation** work: retire a signing key,
start signing with a new one under a new ID, but keep the old public key in the
verifier so the history it signed still verifies. More in §3.2.

### 2.8 `EntryHash` — canonical, deterministic hashing

```go
func EntryHash(e *devicelogv1.LogEntry) ([]byte, error) {
	c := proto.Clone(e).(*devicelogv1.LogEntry)
	c.Audit = nil
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	return h[:], nil
}
```

This four-line function is the heart of the package, and every line is load-bearing.

> **Go concept — `proto.Clone` and why we copy.** We must hash the entry *without*
> its `Audit` block (the block holds the hash, so hashing it would be circular),
> but we must not mutate the caller's entry. `proto.Clone(e)` returns a deep
> copy as `proto.Message` (an interface), which we type-assert back to the
> concrete `*devicelogv1.LogEntry`. Then `c.Audit = nil` clears the copy's audit
> field, leaving the original untouched — clearing a field is just assigning the
> zero value (`nil` for a pointer/message field).

> **Go concept — deterministic marshaling.** `proto.MarshalOptions{Deterministic:
> true}.Marshal(c)` serialises the message to `[]byte` with a **stable byte
> order** (notably, map fields are emitted in sorted key order). Protobuf's
> default marshal makes *no* ordering guarantee, so two encodings of the same
> message can differ byte-for-byte — which would produce two different hashes for
> identical content. `Deterministic: true` is what makes the hash reproducible,
> so re-verification years later yields the same digest. Here
> `proto.MarshalOptions{…}` is a **struct literal used as a configured method
> receiver** — you set the option field, then call `.Marshal` on that value.

> **Go concept — `sha256.Sum256` returns a fixed array `[32]byte`.** This is the
> single most important Go type lesson in this doc. `sha256.Sum256(b)` returns a
> **`[32]byte`** — a fixed-size **array**, *not* a slice. An array's length is
> **part of its type**: `[32]byte` and `[16]byte` are different, incompatible
> types, and an array is a **value** (assigning or passing it copies all 32
> bytes, like a C++ `std::array<byte,32>`). A **slice** `[]byte`, by contrast, is
> a header pointing at a backing array, sized at runtime (like a `std::span` or a
> view into a `std::vector`). The stdlib returns an array here so the result can
> live on the stack with no allocation.

> **Go concept — the `h[:]` slice-of-array trick.** Every downstream call
> (`ed25519.Sign`, `bytes.Equal`, storage) wants a `[]byte`, but `h` is a
> `[32]byte`. `h[:]` **slices the whole array**, producing a `[]byte` header that
> *views* `h`'s storage without copying — the standard bridge from array to
> slice. (Watch the lifetime: the slice keeps the array alive; returning `h[:]`
> is fine because escape analysis moves `h` to the heap, exactly as with `&T` in
> doc 02.)

### 2.9 `ChainSign` — building the audit block and the chain

```go
func ChainSign(e *devicelogv1.LogEntry, prev []byte, s *Signer) error {
	h, err := EntryHash(e)
	if err != nil {
		return err
	}
	e.Audit = &devicelogv1.Audit{
		EntryHash: h,
		PrevHash:  prev,
		Signature: s.Sign(h),
		KeyId:     s.keyID,
	}
	return nil
}
```

`ChainSign` hashes the entry, then fills the `Audit` block with four things: the
entry's own hash, the **previous** entry's hash (`prev`, which is `nil` for a
device's first entry — a `nil` slice is a perfectly valid empty value), the
Ed25519 signature over the hash, and the ID of the key that signed. Linking
`PrevHash` to the prior entry's `EntryHash` is what forms the **chain** (§3.1).

### 2.10 `VerifyEntry` — recompute, compare, verify

```go
func VerifyEntry(e *devicelogv1.LogEntry, v *Verifier) error {
	if e.Audit == nil {
		return fmt.Errorf("entry %s has no audit block", e.EntryId)
	}
	h, err := EntryHash(e)
	if err != nil {
		return err
	}
	if !bytes.Equal(h, e.Audit.EntryHash) {
		return fmt.Errorf("entry %s content does not match its recorded hash", e.EntryId)
	}
	return v.Verify(e.Audit.KeyId, e.Audit.EntryHash, e.Audit.Signature)
}
```

Verification reverses signing in three checks: the block must exist; the
**recomputed** hash must match the **recorded** one (catches content tampering);
and the signature must verify under the recorded key ID (catches forgery).

> **Go concept — `bytes.Equal` vs `==`.** You cannot compare two `[]byte` with
> `==` — slices are only comparable to `nil`, and `==` on them is a compile
> error. `bytes.Equal(a, b)` does the element-wise comparison. (For *arrays*,
> `==` *does* work, because array length and element type are fixed — another way
> arrays and slices differ. Since `h` was sliced to `[]byte`, we need
> `bytes.Equal`.) Note this is a plain equality check, not a constant-time
> compare; that is correct here because both hashes are public, non-secret data.

---

## 3. Deep dives

### 3.1 Canonical hashing, determinism, and the hash chain

Two independent properties make this scheme trustworthy.

**Canonicality** ensures the *same content* always hashes to the *same digest*,
across machines and across time. Getting there needs all three moves in
`EntryHash`: **clone** so we do not disturb the caller; **nil the audit** so the
hash is over content only, never over itself; and **deterministic marshal** so
byte order is fixed. Drop any one and verification becomes flaky — the classic
symptom being "verifies here, fails on the server" from non-deterministic map
ordering in the wire bytes.

**Chaining** turns a set of signed entries into ordered, gap-proof *evidence*.
Each entry stores `PrevHash = (previous entry's EntryHash)`. To rewrite history
an attacker would have to re-sign not just the edited entry but every entry after
it (each depends on the one before) — and they cannot, because they lack the
private key. So the chain gives you two guarantees at once: **integrity** (a
changed entry's recomputed hash won't match) and **ordering/completeness** (a
deleted or reordered entry breaks a `PrevHash` link at a provable index). This is
tamper *evidence*, not tamper *resistance*: you cannot stop someone editing a
database row, but you can make the edit undeniable.

### 3.2 Key rotation via key IDs

Keys must be rotatable — compromise, policy, or age all force replacement — yet
old entries were signed by old keys and must still verify. The design records,
in every entry, the `KeyId` that signed it, and makes the `Verifier` a
`map[keyID]publicKey` holding *every* key that was ever valid. When you rotate,
you (a) start signing with a new `Signer` under a new ID, and (b) leave the
retired public key in the verifier map. `VerifyEntry` reads the entry's own
`KeyId` and looks up the matching public key, so a 2024 entry verifies under the
2024 key and a 2026 entry under the 2026 key — no re-signing of history. This is
why `Verify` takes the key ID as its first argument rather than the `Verifier`
assuming one global key.

---

## 4. Idioms & gotchas

- **Array vs slice is a real type distinction.** `[32]byte` (value, fixed,
  `==`-comparable) is not `[]byte` (header, runtime-sized, needs `bytes.Equal`).
  `h[:]` bridges array → slice without copying; internalise this — it recurs
  everywhere hashes meet I/O.
- **Comma-ok everywhere.** Map lookups (`pub, ok := v.keys[id]`) and type
  assertions (`priv, ok := key.(T)`) both use it. Prefer it over the panicking
  bare forms whenever the input could legitimately be wrong.
- **`nil` slices are valid.** `prev` is `nil` for a device's first entry, and a
  `nil` `[]byte` behaves as an empty one — no special-casing needed.
- **`==` on slices doesn't compile.** Reach for `bytes.Equal`. Slices compare
  only against `nil`.
- **`crypto/rand`, not `math/rand`.** For any key or nonce, the import path is
  your only guard — one is secure, the other is not.
- **Determinism is not the default.** Plain `proto.Marshal` can reorder bytes;
  you must opt into `Deterministic: true` for anything you hash or sign.
- **Keys are values, but share storage.** Passing a key by value copies a cheap
  header, not the bytes — fast, but treat key bytes as read-only.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why does `EntryHash` return `h[:]` instead of `h`? What is the
   type of each, and which downstream calls force the choice?
2. **Recall.** In `LoadPrivatePEM`, what happens with `priv, ok := key.(…)` if
   the file holds an RSA key? How would the bare form `key.(ed25519.PrivateKey)`
   behave differently?
3. **Apply.** Add a `VerifyChain(entries []*LogEntry, v *Verifier) error` that
   verifies each entry *and* checks that each `PrevHash` equals the prior entry's
   `EntryHash`. Which existing functions do you reuse, and where does
   `bytes.Equal` come in?
4. **Apply.** `Signer.Public()` uses a bare type assertion. Rewrite it defensively
   with the comma-ok form returning `(ed25519.PublicKey, error)`. Is the extra
   safety worth it here — why or why not?
5. **Extend.** Write a `MarshalPublicPEM` round-trip test: generate a pair, marshal
   the public key, load it back with `LoadPublicPEM`, and assert equality. Which
   comparison do you use for two `ed25519.PublicKey` values, and why not `==`?
6. **Hero.** Suppose you must swap SHA-256 for SHA-512. Which single line changes,
   what does `Sum512` return, and why does *no* downstream code (`Sign`,
   `bytes.Equal`, storage) need to change? (Hint: the `h[:]` abstraction.)

---

## 6. Recap & next

You now understand Go's byte-slice foundation and the `crypto/*` standard
library, the hard-edged distinction between **arrays** (`[32]byte`) and
**slices** (`[]byte`) with the `h[:]` bridge between them, type assertions in
both forms, PEM/DER key serialization, and how canonical hashing plus Ed25519
signatures plus a per-device hash chain combine into tamper *evidence*. These
byte-and-crypto skills carry directly into every component that stores or moves
data.

**Next:** [05 — license](05-license.md), where you meet `encoding/json`, the
`time` package, `sync.Mutex`, an `http.Client`, struct embedding, and sentinel
error design — the authentication layer that decides whether devlogd runs at all.
