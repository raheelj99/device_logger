package sign

import (
	"bytes"
	"testing"

	devicelogv1 "devlog/api/gen/devicelog/v1"
)

func newTestSigner(t *testing.T) (*Signer, *Verifier) {
	t.Helper()
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	s := NewSigner(priv, "test-1")
	v := NewVerifier()
	v.Add("test-1", pub)
	return s, v
}

func entry(msg string) *devicelogv1.LogEntry {
	return &devicelogv1.LogEntry{
		EntryId:  "01TEST",
		DeviceId: "station-01",
		Message:  msg,
		Attributes: map[string]string{
			"b": "2", "a": "1", "c": "3",
		},
	}
}

func TestSignAndVerify(t *testing.T) {
	s, v := newTestSigner(t)
	e := entry("hello")
	if err := ChainSign(e, nil, s); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEntry(e, v); err != nil {
		t.Fatalf("expected valid entry, got %v", err)
	}
}

func TestTamperDetected(t *testing.T) {
	s, v := newTestSigner(t)
	e := entry("original")
	if err := ChainSign(e, nil, s); err != nil {
		t.Fatal(err)
	}
	e.Message = "altered after signing"
	if err := VerifyEntry(e, v); err == nil {
		t.Fatal("tampered entry verified successfully")
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	s, _ := newTestSigner(t)
	_, other := newTestSigner(t)
	e := entry("hello")
	if err := ChainSign(e, nil, s); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEntry(e, other); err == nil {
		t.Fatal("entry signed with unknown key verified successfully")
	}
}

func TestDeterministicHash(t *testing.T) {
	h1, err := EntryHash(entry("same"))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := EntryHash(entry("same"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h1, h2) {
		t.Fatal("identical entries produced different hashes")
	}
}

func TestChainLinks(t *testing.T) {
	s, _ := newTestSigner(t)
	first := entry("first")
	if err := ChainSign(first, nil, s); err != nil {
		t.Fatal(err)
	}
	second := entry("second")
	if err := ChainSign(second, first.Audit.EntryHash, s); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.Audit.PrevHash, first.Audit.EntryHash) {
		t.Fatal("chain link not recorded")
	}
}
