// lictl issues and inspects Ed25519 keypairs and signed license files.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"devlog/internal/license"
	"devlog/internal/sign"
)

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
	case "keygen":
		return keygen(args[1:])
	case "issue":
		return issue(args[1:])
	case "inspect":
		return inspect(args[1:])
	default:
		return fmt.Errorf("unknown command %q (want keygen, issue, or inspect)", args[0])
	}
}

// keygen writes <name>.key / <name>.pub PEM files, used both for the license
// issuer identity and for devlogd's entry-signing key.
func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	outDir := fs.String("out-dir", "deploy/keys", "directory for the keypair")
	name := fs.String("name", "issuer", "base name for the key files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pub, priv, err := sign.GenerateKeyPair()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		return err
	}
	privPEM, err := sign.MarshalPrivatePEM(priv)
	if err != nil {
		return err
	}
	pubPEM, err := sign.MarshalPublicPEM(pub)
	if err != nil {
		return err
	}
	privPath := filepath.Join(*outDir, *name+".key")
	pubPath := filepath.Join(*outDir, *name+".pub")
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (keep secret) and %s\n", privPath, pubPath)
	return nil
}

func issue(args []string) error {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	keyFile := fs.String("key", "deploy/keys/issuer.key", "issuer private key (PEM)")
	subject := fs.String("subject", "", "device id this license is bound to, or * for any")
	features := fs.String("features", "ingest,query", "comma-separated feature grants")
	days := fs.Int("days", 365, "validity in days from now")
	maxSessions := fs.Int("max-sessions", 0, "concurrent device sessions allowed (0 = unlimited)")
	out := fs.String("out", "", "output .lic file (default <subject>.lic)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *subject == "" {
		return fmt.Errorf("-subject is required")
	}
	priv, err := sign.LoadPrivatePEM(*keyFile)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	lic := license.License{
		ID:          fmt.Sprintf("lic-%s-%d", *subject, now.Unix()),
		Subject:     *subject,
		Features:    strings.Split(*features, ","),
		NotBefore:   now,
		NotAfter:    now.AddDate(0, 0, *days),
		MaxSessions: *maxSessions,
	}
	signed, err := license.Issue(lic, priv)
	if err != nil {
		return err
	}
	token, err := signed.Token()
	if err != nil {
		return err
	}
	path := *out
	if path == "" {
		path = strings.ReplaceAll(*subject, "*", "any") + ".lic"
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return err
	}
	fmt.Printf("issued %s for %q (features %s, expires %s) → %s\n",
		lic.ID, lic.Subject, *features, lic.NotAfter.Format(time.RFC3339), path)
	return nil
}

func inspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	pubFile := fs.String("pub", "deploy/keys/issuer.pub", "issuer public key to verify against (empty to skip)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: lictl inspect [-pub issuer.pub] <file.lic>")
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	signed, err := license.Parse(strings.TrimSpace(string(raw)))
	if err != nil {
		return err
	}
	l := signed.License
	fmt.Printf("id:           %s\nsubject:      %s\nfeatures:     %s\nnot_before:   %s\nnot_after:    %s\nmax_sessions: %d\n",
		l.ID, l.Subject, strings.Join(l.Features, ","), l.NotBefore.Format(time.RFC3339), l.NotAfter.Format(time.RFC3339), l.MaxSessions)
	if *pubFile != "" {
		pub, err := sign.LoadPublicPEM(*pubFile)
		if err != nil {
			return err
		}
		if err := license.NewVerifier(pub).Verify(signed, "", "", time.Now()); err != nil {
			return fmt.Errorf("verification FAILED: %w", err)
		}
		fmt.Println("signature:    VALID")
	}
	return nil
}
