package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// A signature binds its kind: a signed installer must not pass as an agent
// binary or a VM image (or the reverse), even for the same version and hash.
func TestSignatureDoesNotCrossKinds(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const version = "0.1.0"
	const sum = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	kinds := []string{KindAgent, KindImage, KindInstaller}
	for _, signed := range kinds {
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, Message(signed, version, sum)))
		for _, checked := range kinds {
			err := verify(pub, checked, version, sum, sig)
			if signed == checked && err != nil {
				t.Errorf("%s signature rejected as %s: %v", signed, checked, err)
			}
			if signed != checked && err == nil {
				t.Errorf("%s signature accepted as %s", signed, checked)
			}
		}
		if verify(pub, signed, "0.1.1", sum, sig) == nil {
			t.Errorf("%s %s signature accepted for another version", signed, version)
		}
	}
}
