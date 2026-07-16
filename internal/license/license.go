// Package license implements session authentication: Ed25519-signed license
// files verified offline, optionally corroborated online against a license
// server. The license itself is the credential — it is presented as the MQTT
// password and as the gRPC bearer token.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

const (
	FeatureIngest = "ingest"
	FeatureQuery  = "query"
)

type License struct {
	ID          string    `json:"lic_id"`
	Subject     string    `json:"subject"` // device id, or "*" for any
	Features    []string  `json:"features"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	MaxSessions int       `json:"max_sessions"` // 0 = unlimited
}

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

func Issue(l License, priv ed25519.PrivateKey) (*Signed, error) {
	b, err := l.canonical()
	if err != nil {
		return nil, err
	}
	return &Signed{License: l, Signature: ed25519.Sign(priv, b)}, nil
}

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

func (s *Signed) HasFeature(f string) bool {
	return slices.Contains(s.License.Features, f)
}

// Verifier checks licenses offline against the issuer's public key.
type Verifier struct {
	pub  ed25519.PublicKey
	skew time.Duration
}

func NewVerifier(pub ed25519.PublicKey) *Verifier {
	return &Verifier{pub: pub, skew: 5 * time.Minute}
}

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
