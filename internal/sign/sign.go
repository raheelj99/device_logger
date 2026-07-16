package sign

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"

	"google.golang.org/protobuf/proto"

	devicelogv1 "devlog/api/gen/devicelog/v1"
)

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

func (s *Signer) KeyID() string               { return s.keyID }
func (s *Signer) Public() ed25519.PublicKey   { return s.priv.Public().(ed25519.PublicKey) }
func (s *Signer) Sign(digest []byte) []byte   { return ed25519.Sign(s.priv, digest) }

// Verifier resolves key IDs to public keys, so rotated keys keep verifying
// the history they signed.
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

// EntryHash computes the canonical SHA-256 of an entry: the Audit block is
// excluded (it holds the hash itself) and marshaling is deterministic so the
// same content always produces the same digest.
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

// ChainSign fills e.Audit: hash the entry, link it to the device's previous
// entry hash, and sign. prev is nil for the first entry of a device.
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

// VerifyEntry recomputes the canonical hash and checks it and its signature
// against the stored Audit block.
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
