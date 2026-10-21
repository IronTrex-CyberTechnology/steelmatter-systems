// Command smkeys generates synthetic ES256 development keys and a local
// trust store. Keys are for synthetic data only; never use them elsewhere.
//
//	smkeys generate -dir .local/keys -name steelmatter-service -purpose SM-PERMIT+JWS -purpose SM-EXECUTION+JWS
//	smkeys list -dir .local/keys
//	smkeys retire -dir .local/keys -name steelmatter-service -at 2026-10-13T00:00:00Z
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/keys"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: smkeys generate|list -dir DIR [-name NAME]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	dir := fs.String("dir", ".local/keys", "key directory")
	name := fs.String("name", "", "key name (generate, retire)")
	at := fs.String("at", "", "retirement time, RFC 3339 (retire; default now)")
	var purposes multiFlag
	fs.Var(&purposes, "purpose", "JWS typ this key may sign, e.g. SM-PERMIT+JWS (repeatable)")
	_ = fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "generate":
		if *name == "" {
			fmt.Fprintln(os.Stderr, "smkeys: -name is required")
			os.Exit(2)
		}
		k, err := keys.Provision(*dir, *name, purposes, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "smkeys:", err)
			os.Exit(1)
		}
		fmt.Printf("generated %s kid=%s (private key %s, mode 0600)\n", k.Name, k.Kid, filepath.Join(*dir, k.Name+".key.pem"))
	case "retire":
		path := filepath.Join(*dir, "trust-store.json")
		ts, err := keys.LoadTrustStore(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "smkeys:", err)
			os.Exit(1)
		}
		when := time.Now()
		if *at != "" {
			if when, err = time.Parse(time.RFC3339, *at); err != nil {
				fmt.Fprintln(os.Stderr, "smkeys: -at must be RFC 3339")
				os.Exit(2)
			}
		}
		if err := ts.Retire(*name, when); err != nil {
			fmt.Fprintln(os.Stderr, "smkeys:", err)
			os.Exit(1)
		}
		if err := ts.Save(path); err != nil {
			fmt.Fprintln(os.Stderr, "smkeys:", err)
			os.Exit(1)
		}
		fmt.Printf("retired %s from %s; it still verifies records signed before then\n", *name, when.UTC().Format(time.RFC3339))
	case "list":
		ts, err := keys.LoadTrustStore(filepath.Join(*dir, "trust-store.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "smkeys:", err)
			os.Exit(1)
		}
		for _, k := range ts.Keys {
			fmt.Printf("%-24s %s %s synthetic=%v purposes=%s\n", k.Name, k.Kid, k.Alg, k.Synthetic, strings.Join(k.Purposes, ","))
		}
	default:
		fmt.Fprintln(os.Stderr, "smkeys: unknown command", os.Args[1])
		os.Exit(2)
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
