//go:build e2e

package gitlabe2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// TestSigning checks that GitLab verifies the SSH signature of a commit
// touchmark signs with the writer's key, for a service account too.
// A fresh ed25519 key is registered as a signing key of the writer by
// root (POST /users/:id/keys with usage_type signing), and distribute runs
// with it in TOUCHMARK_<ID>_SIGNING_KEY: touchmark signs its commit in the
// SSHSIG format itself. GitLab's signature API then says whether it
// verifies the commit: the key must belong to the committer, whose email
// must be one of the account's verified emails.
func TestSigning(t *testing.T) {
	e := needLive(t)
	s := newScenario(t, e)
	key, signer := newSigningKey(t)
	e.Redact.Add(string(key))
	var added struct {
		ID        int64  `json:"id"`
		UsageType string `json:"usage_type"`
	}
	resp := e.api(e.Root).do(t, http.MethodPost, fmt.Sprintf("/users/%d/keys", e.Writer.ID), map[string]any{
		"title": "touchmark-e2e signing", "key": signer.PublicKey(), "usage_type": "signing",
	})
	decode(t, "keys", resp, &added)
	finding(t, "signing-key", "root's POST /users/<writer>/keys with usage_type signing for the writer (%s): HTTP %d, usage_type %q",
		e.Accounts, resp.Status, added.UsageType)
	if resp.Status != http.StatusCreated {
		t.Skipf("GitLab does not take a signing key for the writer (%s): HTTP %d %s", e.Accounts, resp.Status, e.snippet(resp.Body))
	}
	t.Cleanup(func() {
		e.api(e.Root).do(t, http.MethodDelete, fmt.Sprintf("/users/%d/keys/%d", e.Writer.ID, added.ID), nil)
	})

	reportFile := filepath.Join(s.dir, "report.json")
	out := s.run(0, map[string]string{s.envVar("WRITE_TOKEN"): e.Writer.Token, s.envVar("SIGNING_KEY"): string(key)},
		"distribute", "--hub", s.hub, "--hub-fp", s.fp, "--format", "json", "--report", reportFile)
	rep := s.decode(out)
	s.want("the signed run", rep, s.expect(map[string]string{"alpha": "opened: #1", "beta": "opened: #1", "delta": "opened: #1"}))
	mr := s.mr("alpha", 1)
	s.checkCommit("alpha", mr)
	var sig struct {
		SignatureType      string `json:"signature_type"`
		VerificationStatus string `json:"verification_status"`
		CommitSource       string `json:"commit_source"`
		Key                *struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"key"`
		User *apiUser `json:"user"`
	}
	resp = e.api(e.Root).do(t, http.MethodGet, fmt.Sprintf("/projects/%d/repository/commits/%s/signature", s.ids["alpha"], mr.SHA), nil)
	decode(t, "signature", resp, &sig)
	c := s.commit("alpha", mr.SHA)
	signedBy := "none"
	if sig.User != nil {
		signedBy = sig.User.Username
	}
	finding(t, "ssh-signature", "touchmark's commit by %s <%s> signed with the writer's registered key: GET .../signature HTTP %d, type %q, status %q, user %s",
		c.CommitterName, c.CommitterEmail, resp.Status, sig.SignatureType, sig.VerificationStatus, signedBy)
	switch {
	case resp.Status != http.StatusOK || sig.SignatureType != "SSH":
		t.Errorf("the commit carries no SSH signature GitLab reads: HTTP %d, type %q", resp.Status, sig.SignatureType)
	case e.Accounts == accountsService && sig.VerificationStatus != "verified":
		t.Errorf("the service account's signature is %q, want verified", sig.VerificationStatus)
	}
	s.scanPlatform()
}

// newSigningKey returns a new unencrypted ed25519 key in OpenSSH's private
// key format (PROTOCOL.key), as ssh-keygen writes it, and touchmark's
// signer of it.
func newSigningKey(t *testing.T) ([]byte, *sshsig.Signer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	str := func(b, s []byte) []byte { return append(binary.BigEndian.AppendUint32(b, uint32(len(s))), s...) }
	pubBlob := str(str(nil, []byte("ssh-ed25519")), pub)
	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		t.Fatal(err)
	}
	sec := append(append([]byte(nil), check[:]...), check[:]...)
	sec = str(sec, []byte("ssh-ed25519"))
	sec = str(sec, pub)
	sec = str(sec, priv)
	sec = str(sec, []byte("touchmark-e2e"))
	for i := byte(1); len(sec)%8 != 0; i++ {
		sec = append(sec, i)
	}
	b := []byte("openssh-key-v1\x00")
	b = str(b, []byte("none"))
	b = str(b, []byte("none"))
	b = str(b, nil)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = str(b, pubBlob)
	b = str(b, sec)
	key := pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b})
	signer, err := sshsig.ParsePrivateKey(key)
	if err != nil {
		t.Fatalf("the generated key: %v", err)
	}
	return key, signer
}
