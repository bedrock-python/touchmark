// Package sshsig signs git commits with an ed25519 SSH key in the SSHSIG
// format git uses for gpg.format=ssh: a signature that costs no API call,
// unlike a commit the platform signs.
//
// It uses only the standard library: it reads unencrypted OpenSSH private
// keys (openssh-key-v1) holding one ed25519 key, and writes the armored
// "-----BEGIN SSH SIGNATURE-----" block (namespace "git", hash sha512).
// ed25519 signatures are deterministic, so a commit built twice from the
// same input has the same id.
//
// The formats are OpenSSH's PROTOCOL.key (the private key file) and
// PROTOCOL.sshsig (the signature); the output is byte for byte what
// `ssh-keygen -Y sign -n <namespace>` prints for the same key and message,
// without the final newline.
package sshsig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// Namespace is the SSHSIG namespace git uses for commits.
const Namespace = "git"

// Formats of PROTOCOL.key and PROTOCOL.sshsig.
const (
	pemType     = "OPENSSH PRIVATE KEY"
	keyMagic    = "openssh-key-v1\x00"
	keyType     = "ssh-ed25519"
	blockSize   = 8 // cipher "none"
	sigMagic    = "SSHSIG"
	sigVersion  = 1
	hashAlg     = "sha512"
	armorBegin  = "-----BEGIN SSH SIGNATURE-----"
	armorEnd    = "-----END SSH SIGNATURE-----"
	armorWidth  = 70 // ssh-keygen wraps base64 at 70 columns
	maxKeyBytes = 64 << 10
)

// Signer signs with one ed25519 key. It is safe for concurrent use.
type Signer struct {
	priv ed25519.PrivateKey
	// pubBlob is the public key in SSH wire form: string "ssh-ed25519",
	// string key.
	pubBlob []byte
}

// ParsePrivateKey reads an OpenSSH private key ("-----BEGIN OPENSSH PRIVATE
// KEY-----"). It rejects encrypted keys, keys that are not ed25519, files
// holding several keys, and keys whose check integers differ.
//
// The input is one PEM block, surrounding whitespace aside. It also
// rejects PEM headers, a public key that differs from the private one, a
// private key that does not derive its public key, and malformed padding.
// Errors never quote key material.
func ParsePrivateKey(data []byte) (*Signer, error) {
	if len(data) > maxKeyBytes {
		return nil, errors.New("sshsig: the key is too large")
	}
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
		return nil, errors.New("sshsig: not a PEM private key")
	}
	block, rest := pem.Decode(trimmed)
	switch {
	case block == nil:
		return nil, errors.New("sshsig: not a PEM private key")
	case block.Type != pemType:
		return nil, fmt.Errorf("sshsig: a %.40q PEM block, not an OpenSSH private key", block.Type)
	case len(block.Headers) > 0:
		return nil, errors.New("sshsig: PEM headers (an encrypted legacy key?) are not supported")
	case len(bytes.TrimSpace(rest)) > 0:
		return nil, errors.New("sshsig: data after the private key")
	}
	return parseKey(block.Bytes)
}

// parseKey parses the openssh-key-v1 structure.
func parseKey(b []byte) (*Signer, error) {
	malformed := errors.New("sshsig: malformed OpenSSH private key")
	if !bytes.HasPrefix(b, []byte(keyMagic)) {
		return nil, errors.New("sshsig: not an openssh-key-v1 private key")
	}
	r := reader{b: b[len(keyMagic):]}
	cipher, kdf, kdfOptions := r.str(), r.str(), r.str()
	n := r.u32()
	switch {
	case r.err:
		return nil, malformed
	case string(cipher) != "none" || string(kdf) != "none" || len(kdfOptions) != 0:
		return nil, errors.New("sshsig: the key is encrypted: use a key without a passphrase")
	case n != 1:
		return nil, fmt.Errorf("sshsig: the file holds %d keys, want one", n)
	}
	pubBlob := r.str()
	private := r.str()
	if r.err || len(r.b) != 0 {
		return nil, malformed
	}
	pub, err := parsePublic(pubBlob)
	if err != nil {
		return nil, err
	}
	if len(private)%blockSize != 0 {
		return nil, malformed
	}
	p := reader{b: private}
	check1, check2 := p.u32(), p.u32()
	typ := p.str()
	if p.err {
		return nil, malformed
	}
	if check1 != check2 {
		return nil, errors.New("sshsig: the key's check integers differ (a corrupt or encrypted key)")
	}
	if string(typ) != keyType {
		return nil, fmt.Errorf("sshsig: a %.40q key, want ed25519", typ)
	}
	pk, sk, _ := p.str(), p.str(), p.str() // comment ignored
	if p.err || len(pk) != ed25519.PublicKeySize || len(sk) != ed25519.PrivateKeySize {
		return nil, malformed
	}
	for i, c := range p.b {
		if c != byte(i+1) || i >= blockSize {
			return nil, malformed
		}
	}
	if !bytes.Equal(pk, pub) || !bytes.Equal(sk[ed25519.SeedSize:], pk) {
		return nil, errors.New("sshsig: the private key's public half differs from the public key")
	}
	priv := ed25519.NewKeyFromSeed(sk[:ed25519.SeedSize])
	if subtle.ConstantTimeCompare(priv, sk) != 1 {
		return nil, errors.New("sshsig: the private key does not derive its public key")
	}
	return &Signer{priv: priv, pubBlob: clone(pubBlob)}, nil
}

// parsePublic parses an ed25519 public key blob: string "ssh-ed25519",
// string key (32 bytes), nothing after.
func parsePublic(blob []byte) ([]byte, error) {
	r := reader{b: blob}
	typ, key := r.str(), r.str()
	if r.err || len(r.b) != 0 {
		return nil, errors.New("sshsig: malformed public key")
	}
	if string(typ) != keyType {
		return nil, fmt.Errorf("sshsig: a %.40q key, want ed25519", typ)
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, errors.New("sshsig: malformed ed25519 public key")
	}
	return key, nil
}

// PublicKey returns the key in authorized_keys form: "ssh-ed25519 AAAA…".
func (s *Signer) PublicKey() string {
	if s == nil {
		return ""
	}
	return keyType + " " + base64.StdEncoding.EncodeToString(s.pubBlob)
}

// Sign returns the armored SSHSIG signature of message in namespace, lines
// of at most 70 characters, ending with "-----END SSH SIGNATURE-----" and
// no trailing newline.
//
// Per PROTOCOL.sshsig, ed25519 signs "SSHSIG" + string(namespace) +
// string("") + string("sha512") + string(sha512(message)); the blob is
// "SSHSIG" + uint32(1) + string(public key) + string(namespace) +
// string("") + string("sha512") + string(signature), the signature being
// string("ssh-ed25519") + string(64 bytes). The namespace must not be
// empty.
func (s *Signer) Sign(namespace string, message []byte) (string, error) {
	if s == nil || len(s.priv) != ed25519.PrivateKeySize {
		return "", errors.New("sshsig: no key")
	}
	if namespace == "" {
		return "", errors.New("sshsig: empty namespace")
	}
	h := sha512.Sum512(message)
	var signed []byte
	signed = append(signed, sigMagic...)
	signed = putString(signed, []byte(namespace))
	signed = putString(signed, nil) // reserved
	signed = putString(signed, []byte(hashAlg))
	signed = putString(signed, h[:])
	var sig []byte
	sig = putString(sig, []byte(keyType))
	sig = putString(sig, ed25519.Sign(s.priv, signed))

	var blob []byte
	blob = append(blob, sigMagic...)
	blob = binary.BigEndian.AppendUint32(blob, sigVersion)
	blob = putString(blob, s.pubBlob)
	blob = putString(blob, []byte(namespace))
	blob = putString(blob, nil) // reserved
	blob = putString(blob, []byte(hashAlg))
	blob = putString(blob, sig)
	return armor(blob), nil
}

// armor wraps blob the way ssh-keygen does, without the final newline.
func armor(blob []byte) string {
	enc := base64.StdEncoding.EncodeToString(blob)
	var b strings.Builder
	b.WriteString(armorBegin + "\n")
	for len(enc) > armorWidth {
		b.WriteString(enc[:armorWidth] + "\n")
		enc = enc[armorWidth:]
	}
	b.WriteString(enc + "\n")
	b.WriteString(armorEnd)
	return b.String()
}

// putString appends an SSH string: a uint32 length and the bytes.
func putString(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// reader reads SSH wire data; err is set once data runs out.
type reader struct {
	b   []byte
	err bool
}

func (r *reader) u32() uint32 {
	if r.err || len(r.b) < 4 {
		r.err = true
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) str() []byte {
	n := r.u32()
	if r.err || uint64(n) > uint64(len(r.b)) {
		r.err = true
		return nil
	}
	s := r.b[:n]
	r.b = r.b[n:]
	return s
}

// clone returns a copy of b.
func clone(b []byte) []byte { return append([]byte(nil), b...) }
