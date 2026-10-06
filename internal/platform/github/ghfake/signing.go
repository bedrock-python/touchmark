package ghfake

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The fake's "web-flow" signature: GitHub signs web and API commits with
// its own PGP key (docs.github.com/en/authentication/managing-commit-signature-verification/about-commit-signature-verification).
// The fake writes a PGP-armored block whose body is an HMAC of the commit
// payload under a key of the server: only the server that made it
// verifies it, and nothing parses it as a real OpenPGP packet.
const (
	pgpBegin   = "-----BEGIN PGP SIGNATURE-----"
	pgpEnd     = "-----END PGP SIGNATURE-----"
	fakeSigTag = "ghfake-web-flow:"
	sshBegin   = "-----BEGIN SSH SIGNATURE-----"
	sshEnd     = "-----END SSH SIGNATURE-----"
)

// webFlowName and webFlowEmail are the committer of commits GitHub makes
// and signs (observed read-only 2026-09-29 on an App's API commit:
// committer "GitHub <noreply@github.com>", verified, reason "valid").
const (
	webFlowName  = "GitHub"
	webFlowEmail = "noreply@github.com"
)

// webFlowSign returns the fake web-flow signature of payload.
func (s *Server) webFlowSign(payload string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(payload))
	return pgpBegin + "\n\n" + fakeSigTag + base64.StdEncoding.EncodeToString(m.Sum(nil)) + "\n" + pgpEnd
}

// verification is the "verification" object of a commit.
type verification struct {
	verified   bool
	reason     string
	signature  string
	payload    string
	verifiedAt time.Time
}

// json returns the REST form of v.
func (v verification) json() map[string]any {
	out := map[string]any{"verified": v.verified, "reason": v.reason, "signature": nil, "payload": nil, "verified_at": nil}
	if v.signature != "" {
		out["signature"] = v.signature
		out["payload"] = v.payload
	}
	if v.verified {
		out["verified_at"] = fmtTime(v.verifiedAt)
	}
	return out
}

// verify checks the signature of a commit the way GitHub reports it
// (reasons from docs.github.com/en/rest/git/commits): "unsigned" without
// one; "valid" for the fake web-flow signature of this server, and for an
// SSH signature by a key registered as a signing key of the account the
// committer email belongs to; "unknown_key" for an SSH key nobody
// registered; "bad_email" when the key's owner does not own the committer
// email; "invalid" for a signature that does not verify; "malformed_signature"
// for one that does not parse; "unknown_key" for other PGP signatures
// (the fake knows no PGP keys). Called with mu held.
func (s *Server) verify(c commitInfo) verification {
	v := verification{reason: "unsigned"}
	if c.sig == "" {
		return v
	}
	v.signature, v.payload = c.sig, c.payload
	switch {
	case strings.HasPrefix(c.sig, pgpBegin):
		body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(c.sig, pgpBegin), pgpEnd))
		tag, ok := strings.CutPrefix(body, fakeSigTag)
		if !ok {
			v.reason = "unknown_key"
			return v
		}
		want, err := base64.StdEncoding.DecodeString(tag)
		m := hmac.New(sha256.New, s.secret)
		m.Write([]byte(c.payload))
		if err != nil || !hmac.Equal(want, m.Sum(nil)) {
			v.reason = "invalid"
			return v
		}
		v.verified, v.reason, v.verifiedAt = true, "valid", c.committer.when
	case strings.HasPrefix(c.sig, sshBegin):
		pub, err := verifySSHSig(c.sig, []byte(c.payload))
		switch {
		case errors.Is(err, errBadSignature):
			v.reason = "invalid"
			return v
		case err != nil:
			v.reason = "malformed_signature"
			return v
		}
		owner := s.keyOwner(pub)
		switch {
		case owner == nil:
			v.reason = "unknown_key"
		case !owns(owner, c.committer.email):
			v.reason = "bad_email"
		default:
			v.verified, v.reason, v.verifiedAt = true, "valid", c.committer.when
		}
	default:
		v.reason = "unknown_signature_type"
	}
	return v
}

// keyOwner returns the account that registered an SSH signing key blob.
// Called with mu held.
func (s *Server) keyOwner(blob []byte) *account {
	for _, a := range s.accounts {
		for _, k := range a.signingKeys {
			if bytes.Equal(k, blob) {
				return a
			}
		}
	}
	return nil
}

// owns reports whether email is one of a's addresses.
func owns(a *account, email string) bool {
	if strings.EqualFold(email, a.noreply()) {
		return true
	}
	for _, e := range a.emails {
		if strings.EqualFold(e, email) {
			return true
		}
	}
	return false
}

// errBadSignature is a well-formed SSH signature that does not verify.
var errBadSignature = errors.New("signature does not verify")

// verifySSHSig checks an armored SSHSIG signature of message in the "git"
// namespace (OpenSSH PROTOCOL.sshsig, ed25519 only) and returns the public
// key blob that made it.
func verifySSHSig(armored string, message []byte) ([]byte, error) {
	body := strings.TrimSpace(armored)
	body, ok1 := strings.CutPrefix(body, sshBegin)
	body, ok2 := strings.CutSuffix(strings.TrimSpace(body), sshEnd)
	if !ok1 || !ok2 {
		return nil, errors.New("not an SSH signature")
	}
	blob, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(body), ""))
	if err != nil {
		return nil, fmt.Errorf("SSH signature: %w", err)
	}
	if !bytes.HasPrefix(blob, []byte("SSHSIG")) {
		return nil, errors.New("SSH signature: no magic")
	}
	r := &wire{b: blob[6:]}
	version := r.u32()
	pub := r.str()
	namespace := r.str()
	_ = r.str() // reserved
	hashAlg := r.str()
	sig := r.str()
	if r.bad || version != 1 || string(namespace) != "git" {
		return nil, errors.New("SSH signature: unsupported")
	}
	pr := &wire{b: pub}
	keyType, key := pr.str(), pr.str()
	sr := &wire{b: sig}
	sigType, raw := sr.str(), sr.str()
	if pr.bad || sr.bad || string(keyType) != "ssh-ed25519" || string(sigType) != "ssh-ed25519" ||
		len(key) != ed25519.PublicKeySize {
		return nil, errors.New("SSH signature: not ed25519")
	}
	var digest []byte
	switch string(hashAlg) {
	case "sha512":
		h := sha512.Sum512(message)
		digest = h[:]
	case "sha256":
		h := sha256.Sum256(message)
		digest = h[:]
	default:
		return nil, errors.New("SSH signature: unknown hash")
	}
	var signed []byte
	signed = append(signed, "SSHSIG"...)
	signed = putString(signed, namespace)
	signed = putString(signed, nil)
	signed = putString(signed, hashAlg)
	signed = putString(signed, digest)
	if !ed25519.Verify(ed25519.PublicKey(key), signed, raw) {
		return pub, errBadSignature
	}
	return pub, nil
}

// wire reads SSH wire data.
type wire struct {
	b   []byte
	bad bool
}

func (w *wire) u32() uint32 {
	if w.bad || len(w.b) < 4 {
		w.bad = true
		return 0
	}
	v := binary.BigEndian.Uint32(w.b)
	w.b = w.b[4:]
	return v
}

func (w *wire) str() []byte {
	n := w.u32()
	if w.bad || uint64(n) > uint64(len(w.b)) {
		w.bad = true
		return nil
	}
	s := w.b[:n]
	w.b = w.b[n:]
	return s
}

// putString appends an SSH string.
func putString(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// parseAuthorizedKey parses "ssh-ed25519 AAAA… [comment]" into its blob.
func parseAuthorizedKey(line string) ([]byte, error) {
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "ssh-ed25519" {
		return nil, errors.New("not an ssh-ed25519 public key")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	r := &wire{b: blob}
	if t, k := r.str(), r.str(); r.bad || string(t) != "ssh-ed25519" || len(k) != ed25519.PublicKeySize {
		return nil, errors.New("not an ssh-ed25519 public key")
	}
	return blob, nil
}
