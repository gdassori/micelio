package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// Identity holds the node's ED25519 keypair and derived identifiers.
// The same keypair serves as SSH host key, P2P identity, and message signer.
type Identity struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
	NodeID     string // hex(sha256(public_key))
	SSHSigner  ssh.Signer
}

// Load reads the keypair from dataDir/identity/. If the key files don't
// exist, a new keypair is generated and persisted.
func Load(dataDir string) (*Identity, error) {
	keyDir := filepath.Join(dataDir, "identity")
	privPath := filepath.Join(keyDir, "node.key")
	pubPath := filepath.Join(keyDir, "node.pub")

	privPEM, err := os.ReadFile(privPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading private key: %w", err)
		}
		return generate(keyDir, privPath, pubPath)
	}

	return loadFrom(privPEM)
}

func generate(keyDir, privPath, pubPath string) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating keypair: %w", err)
	}

	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return nil, fmt.Errorf("creating identity dir: %w", err)
	}

	// Save private key as PKCS8 PEM
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshaling private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8,
	})
	if err := os.WriteFile(privPath, privPEM, 0600); err != nil {
		return nil, fmt.Errorf("writing private key: %w", err)
	}

	// Save public key in OpenSSH format
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("converting public key: %w", err)
	}
	pubLine := ssh.MarshalAuthorizedKey(sshPub)
	if err := os.WriteFile(pubPath, pubLine, 0644); err != nil {
		return nil, fmt.Errorf("writing public key: %w", err)
	}

	return fromKeyPair(priv, pub)
}

func loadFrom(privPEM []byte) (*Identity, error) {
	block, _ := pem.Decode(privPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private key")
	}

	rawKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}

	priv, ok := rawKey.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not ED25519")
	}

	pub := priv.Public().(ed25519.PublicKey)
	return fromKeyPair(priv, pub)
}

func fromKeyPair(priv ed25519.PrivateKey, pub ed25519.PublicKey) (*Identity, error) {
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("creating SSH signer: %w", err)
	}

	hash := sha256.Sum256(pub)
	nodeID := hex.EncodeToString(hash[:])

	return &Identity{
		PrivateKey: priv,
		PublicKey:  pub,
		NodeID:     nodeID,
		SSHSigner:  signer,
	}, nil
}
