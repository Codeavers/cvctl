package gitrepo_test

import (
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/gitrepo"
	"golang.org/x/crypto/ssh"
)

func TestNewDeployKeyFormat(t *testing.T) {
	comment := "cvctl-test-key@host"
	privPEM, pubAuth, err := gitrepo.NewDeployKey(comment)
	if err != nil {
		t.Fatalf("unexpected error generating deploy key: %v", err)
	}

	pubStr := string(pubAuth)
	if !strings.HasPrefix(pubStr, "ssh-ed25519 ") {
		t.Errorf("expected public key to start with 'ssh-ed25519 ', got %q", pubStr)
	}
	if !strings.HasSuffix(strings.TrimSpace(pubStr), comment) {
		t.Errorf("expected public key to end with comment %q, got %q", comment, pubStr)
	}
	if !strings.HasSuffix(pubStr, "\n") {
		t.Errorf("expected public key to end with newline")
	}

	privStr := string(privPEM)
	if !strings.HasPrefix(privStr, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("expected private key to start with '-----BEGIN OPENSSH PRIVATE KEY-----', got %q", privStr)
	}
	if !strings.HasSuffix(strings.TrimSpace(privStr), "-----END OPENSSH PRIVATE KEY-----") {
		t.Errorf("expected private key to end with '-----END OPENSSH PRIVATE KEY-----', got %q", privStr)
	}

	// Verify that the keys are cryptographically valid OpenSSH keys
	parsedPub, parsedComment, _, _, err := ssh.ParseAuthorizedKey(pubAuth)
	if err != nil {
		t.Fatalf("failed to parse authorized key: %v", err)
	}
	if parsedPub.Type() != "ssh-ed25519" {
		t.Errorf("expected key type ssh-ed25519, got %s", parsedPub.Type())
	}
	if parsedComment != comment {
		t.Errorf("expected parsed comment %q, got %q", comment, parsedComment)
	}

	_, err = ssh.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatalf("failed to parse private key PEM: %v", err)
	}
}

func TestNewDeployKeyEmptyComment(t *testing.T) {
	privPEM, pubAuth, err := gitrepo.NewDeployKey("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pubStr := string(pubAuth)
	if !strings.HasPrefix(pubStr, "ssh-ed25519 ") {
		t.Errorf("expected public key to start with 'ssh-ed25519 ', got %q", pubStr)
	}
	if !strings.HasSuffix(pubStr, "\n") {
		t.Errorf("expected public key to end with newline")
	}

	privStr := string(privPEM)
	if !strings.HasPrefix(privStr, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("expected private key to start with '-----BEGIN OPENSSH PRIVATE KEY-----', got %q", privStr)
	}

	_, _, _, _, err = ssh.ParseAuthorizedKey(pubAuth)
	if err != nil {
		t.Fatalf("failed to parse authorized key: %v", err)
	}
	_, err = ssh.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatalf("failed to parse private key: %v", err)
	}
}
