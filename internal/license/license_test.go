package license

import (
	"testing"
	"time"

	"devlog/internal/sign"
)

func issueTest(t *testing.T, l License) (*Signed, *Verifier) {
	t.Helper()
	pub, priv, err := sign.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Issue(l, priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, NewVerifier(pub)
}

func validLicense() License {
	return License{
		ID:        "lic-001",
		Subject:   "station-01",
		Features:  []string{FeatureIngest, FeatureQuery},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
	}
}

func TestTokenRoundTrip(t *testing.T) {
	s, v := issueTest(t, validLicense())
	token, err := s.Token()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(parsed, "station-01", FeatureIngest, time.Now()); err != nil {
		t.Fatalf("expected valid license, got %v", err)
	}
}

func TestExpiredRejected(t *testing.T) {
	l := validLicense()
	l.NotAfter = time.Now().Add(-time.Hour)
	s, v := issueTest(t, l)
	if err := v.Verify(s, "station-01", "", time.Now()); err == nil {
		t.Fatal("expired license verified successfully")
	}
}

func TestWrongSubjectRejected(t *testing.T) {
	s, v := issueTest(t, validLicense())
	if err := v.Verify(s, "intruder-99", "", time.Now()); err == nil {
		t.Fatal("license accepted for wrong subject")
	}
}

func TestWildcardSubjectAccepted(t *testing.T) {
	l := validLicense()
	l.Subject = "*"
	s, v := issueTest(t, l)
	if err := v.Verify(s, "any-device", FeatureQuery, time.Now()); err != nil {
		t.Fatalf("wildcard license rejected: %v", err)
	}
}

func TestMissingFeatureRejected(t *testing.T) {
	l := validLicense()
	l.Features = []string{FeatureIngest}
	s, v := issueTest(t, l)
	if err := v.Verify(s, "station-01", FeatureQuery, time.Now()); err == nil {
		t.Fatal("license accepted for feature it does not grant")
	}
}

func TestTamperedLicenseRejected(t *testing.T) {
	s, v := issueTest(t, validLicense())
	s.License.NotAfter = s.License.NotAfter.Add(365 * 24 * time.Hour)
	if err := v.Verify(s, "station-01", "", time.Now()); err == nil {
		t.Fatal("tampered license verified successfully")
	}
}
