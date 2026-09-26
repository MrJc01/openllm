package ssh

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// generateKeyPair deve produzir par ed25519 parseável (OpenSSH PEM), com a
// chave privada a 0600 e pública correspondente. RSA foi abandonado porque
// hosts novos do provedor rejeitam RSA no handshake.
func TestGenerateKeyPairEd25519(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "id_ed25519")
	pubPath := filepath.Join(dir, "id_ed25519.pub")

	if err := generateKeyPair(privPath, pubPath); err != nil {
		t.Fatalf("generateKeyPair: %v", err)
	}

	rawPriv, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read private: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(rawPriv)
	if err != nil {
		t.Fatalf("private key does not parse: %v", err)
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("expected ed25519 key, got %s", signer.PublicKey().Type())
	}

	rawPub, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatalf("read public: %v", err)
	}
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey(rawPub)
	if err != nil {
		t.Fatalf("parse authorized key: %v", err)
	}
	if !bytes.Equal(pubKey.Marshal(), signer.PublicKey().Marshal()) {
		t.Fatal("public key does not match private key")
	}

	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat private: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("private key permissions = %v, want 0600", info.Mode().Perm())
	}
}
