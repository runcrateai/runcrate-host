// Command sign writes <file>.sig for a release artifact. The ed25519 seed
// comes from RUNCRATE_HOST_SIGNING_KEY (base64), e.g.
//
//	RUNCRATE_HOST_SIGNING_KEY=$(security find-generic-password -s runcrate-host-release-signing -w) \
//	  go run ./hack/sign -kind runcrate-host -version 0.1.0 out/runcrate-host-linux-amd64
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/runcrateai/runcrate-host/internal/release"
)

func main() {
	kind := flag.String("kind", "", "runcrate-host or runcrate-vm")
	version := flag.String("version", "", "artifact version")
	sum := flag.String("sha256", "", "sign this sha256 instead of hashing FILE (FILE need not exist; for artifacts built elsewhere)")
	flag.Parse()
	if (*kind != release.KindAgent && *kind != release.KindImage) || *version == "" || flag.NArg() != 1 {
		log.Fatal("usage: sign -kind runcrate-host|runcrate-vm -version V [-sha256 HEX] FILE")
	}
	seed, err := base64.StdEncoding.DecodeString(os.Getenv("RUNCRATE_HOST_SIGNING_KEY"))
	if err != nil || len(seed) != ed25519.SeedSize {
		log.Fatal("RUNCRATE_HOST_SIGNING_KEY must be a base64 ed25519 seed")
	}
	if *sum == "" {
		f, err := os.Open(flag.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			log.Fatal(err)
		}
		f.Close()
		*sum = hex.EncodeToString(h.Sum(nil))
	} else if b, err := hex.DecodeString(*sum); err != nil || len(b) != sha256.Size {
		log.Fatal("-sha256 must be 64 hex digits")
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), release.Message(*kind, *version, *sum))
	if err := release.Verify(*kind, *version, *sum, base64.StdEncoding.EncodeToString(sig)); err != nil {
		log.Fatalf("the key does not match the built-in public key: %v", err)
	}
	if err := os.WriteFile(flag.Arg(0)+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s  %s %s\n", *sum, *kind, *version)
}
