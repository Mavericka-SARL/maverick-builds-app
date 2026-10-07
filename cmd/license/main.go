// cmd/license is the vendor-side tool for maverickbuilds.app license keys.
// Customers never run it; they receive the token it prints.
//
//	license keygen  -out ~/.mavericks/license-signing.key
//	    Generates an Ed25519 key pair. The PRIVATE key goes to -out (mode 0600,
//	    keep it out of every repository); the PUBLIC key is printed so it can be
//	    appended to trustedPublicKeys in pkg/license/pubkey.go (the rotation
//	    steps are there). A deployment cannot be told to trust another key.
//
//	license sign -key ~/.mavericks/license-signing.key -edition enterprise \
//	    -customer "Acme Corp" -expires 2027-12-31 -order ORD-2026-014 \
//	    -agreement "PFA 2026-10" [-deployment acme-prod] [-schedule 2026-10] \
//	    [-contact ops@acme.com] [-features sso,scim] [-id <uuid>] \
//	    [-notes "PO 4711"] [-out acme.license]
//	    Prints (or writes) the signed token. The key implements an accepted
//	    order: -order and -agreement name it, and the order, not the key,
//	    states the rights (docs/LICENSING.md).
//
//	license schedules
//	    Lists the feature schedules this build knows, oldest first.
//
//	license inspect -token <token> | -file acme.license [-pubkey <base64>]
//	    Verifies a token against the compiled-in public keys (or -pubkey) and
//	    prints its claims and whether it is currently valid.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mavericks-engine/mavericks/pkg/license"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "inspect":
		err = inspect(os.Args[2:])
	case "schedules":
		err = listSchedules()
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: license keygen|sign|schedules|inspect [flags]  (run a subcommand with -h for its flags)")
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "file to write the PRIVATE key to (mode 0600); prints to stdout when empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
			return err
		}
		if _, err := os.Stat(*out); err == nil {
			return fmt.Errorf("%s already exists; refusing to overwrite a signing key", *out)
		}
		if err := os.WriteFile(*out, []byte(license.EncodeKey(priv)+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("private key written to %s (keep it secret, back it up, never commit it)\n", *out)
	} else {
		fmt.Printf("private key: %s\n", license.EncodeKey(priv))
	}
	fmt.Printf("public key:  %s\n", license.EncodeKey(pub))
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyFile := fs.String("key", "", "path to the private signing key (required)")
	edition := fs.String("edition", "", "commercial or enterprise (required)")
	customer := fs.String("customer", "", "licensee name (required)")
	contact := fs.String("contact", "", "licensee contact e-mail")
	expires := fs.String("expires", "", "expiry date, YYYY-MM-DD (end of that day, UTC) or RFC 3339 (required)")
	schedule := fs.String("schedule", license.LatestSchedule(), "feature schedule the order was sold under; see `license schedules`")
	order := fs.String("order", "", "order number of the accepted order this key implements (required)")
	agreement := fs.String("agreement", "", "reference of the agreement text the customer accepted (required)")
	deployment := fs.String("deployment", "", "deployment identifier(s) the order covers")
	features := fs.String("features", "", "comma-separated extra features beyond the schedule's edition defaults")
	id := fs.String("id", "", "license id; a new UUID when empty")
	notes := fs.String("notes", "", "free text for your records")
	out := fs.String("out", "", "write the token to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyFile == "" || *edition == "" || *customer == "" || *expires == "" || *order == "" || *agreement == "" {
		return fmt.Errorf("-key, -edition, -customer, -expires, -order and -agreement are required: a key implements an accepted order")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	priv, err := license.DecodePrivateKey(string(raw))
	if err != nil {
		return err
	}
	exp, err := parseExpiry(*expires)
	if err != nil {
		return err
	}
	claims := license.Claims{
		ID:         *id,
		Edition:    license.Edition(*edition),
		Customer:   *customer,
		Contact:    *contact,
		IssuedAt:   time.Now().UTC().Truncate(time.Second),
		ExpiresAt:  exp,
		Schedule:   *schedule,
		Order:      *order,
		Agreement:  *agreement,
		Deployment: *deployment,
		Notes:      *notes,
	}
	if claims.ID == "" {
		claims.ID = uuid.NewString()
	}
	for _, f := range strings.Split(*features, ",") {
		if f = strings.TrimSpace(f); f != "" {
			if _, ok := license.Lookup(license.Feature(f)); !ok {
				return fmt.Errorf("unknown feature %q; known: %s", f, knownFeatures())
			}
			claims.Features = append(claims.Features, license.Feature(f))
		}
	}
	token, err := license.Sign(claims, priv)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(token+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("license %s for %q (%s, expires %s) written to %s\n", claims.ID, claims.Customer, claims.Edition, claims.ExpiresAt.Format(time.RFC3339), *out)
		return nil
	}
	fmt.Println(token)
	return nil
}

func parseExpiry(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.Parse("2006-01-02", s); err == nil {
		return d.Add(24*time.Hour - time.Second).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("expiry %q is neither YYYY-MM-DD nor RFC 3339", s)
}

func knownFeatures() string {
	var names []string
	for _, info := range license.Catalog() {
		names = append(names, string(info.Key))
	}
	return strings.Join(names, ", ")
}

func listSchedules() error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(license.Schedules())
}

func inspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	token := fs.String("token", "", "the license token")
	file := fs.String("file", "", "file holding the token")
	pubkey := fs.String("pubkey", "", "base64 public key to verify against (default: the compiled-in keys)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" && *file == "" {
		return fmt.Errorf("-token or -file is required")
	}
	m := license.Load(license.Options{Key: *token, File: *file, PublicKey: *pubkey})
	st := m.Status()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		return err
	}
	if st.State == license.StateInvalid {
		return fmt.Errorf("invalid: %s", st.Error)
	}
	return nil
}
