package gitrepo

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"

	"golang.org/x/crypto/ssh"
)

// NewDeployKey generates an ed25519 key pair formatted for OpenSSH.
// The private key is returned in OpenSSH PEM format.
// The public key is serialized as an OpenSSH authorized_key with the given comment.
func NewDeployKey(comment string) (privatePEM, publicAuthorized []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, nil, err
	}
	privatePEM = pem.EncodeToMemory(block)

	pubKey, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}

	pubBytes := bytes.TrimRight(ssh.MarshalAuthorizedKey(pubKey), "\r\n")
	if comment != "" {
		pubBytes = append(pubBytes, ' ')
		pubBytes = append(pubBytes, []byte(comment)...)
	}
	pubBytes = append(pubBytes, '\n')

	return privatePEM, pubBytes, nil
}
