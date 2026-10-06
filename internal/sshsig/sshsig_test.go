package sshsig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// keyOpts corrupt or vary a generated key file.
type keyOpts struct {
	cipher, kdf, kdfOptions string
	keys                    int // public keys listed; 1 when zero
	checkDiffers            bool
	typ, pubType            string
	otherPub                bool // the private section names another public key
	badPadding              bool
	trailing                []byte
	comment                 string
}

// seed is the fixed ed25519 seed of the generated test key.
var seed = bytes.Repeat([]byte{0x42}, ed25519.SeedSize)

// marshalKey writes an openssh-key-v1 private key file for seed, as
// ssh-keygen does (PROTOCOL.key), with o's variations. Keys are built here
// rather than stored: no private key sits in the repository.
func marshalKey(t testing.TB, o keyOpts) []byte {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	if o.cipher == "" {
		o.cipher = "none"
	}
	if o.kdf == "" {
		o.kdf = "none"
	}
	if o.keys == 0 {
		o.keys = 1
	}
	if o.typ == "" {
		o.typ = keyType
	}
	if o.pubType == "" {
		o.pubType = keyType
	}
	pubBlob := putString(putString(nil, []byte(o.pubType)), pub)

	var sec []byte
	check := uint32(0x5eed1234)
	sec = binary.BigEndian.AppendUint32(sec, check)
	if o.checkDiffers {
		sec = binary.BigEndian.AppendUint32(sec, check+1)
	} else {
		sec = binary.BigEndian.AppendUint32(sec, check)
	}
	sec = putString(sec, []byte(o.typ))
	inner := pub
	if o.otherPub {
		inner = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)).Public().(ed25519.PublicKey)
	}
	sec = putString(sec, inner)
	sec = putString(sec, priv)
	sec = putString(sec, []byte(o.comment))
	for i := byte(1); len(sec)%blockSize != 0; i++ {
		if o.badPadding {
			sec = append(sec, 0)
		} else {
			sec = append(sec, i)
		}
	}
	var b []byte
	b = append(b, keyMagic...)
	b = putString(b, []byte(o.cipher))
	b = putString(b, []byte(o.kdf))
	b = putString(b, []byte(o.kdfOptions))
	b = binary.BigEndian.AppendUint32(b, uint32(o.keys))
	for i := 0; i < o.keys; i++ {
		b = putString(b, pubBlob)
	}
	b = putString(b, sec)
	b = append(b, o.trailing...)
	return pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: b})
}

// decodeArmor parses an armored signature back into its fields.
func decodeArmor(t *testing.T, armored string) (pubBlob []byte, namespace, hash string, sigType string, sig []byte) {
	t.Helper()
	lines := strings.Split(armored, "\n")
	if lines[0] != armorBegin || lines[len(lines)-1] != armorEnd {
		t.Fatalf("armor = %q", armored)
	}
	for _, l := range lines[1 : len(lines)-1] {
		if len(l) > armorWidth || l == "" {
			t.Fatalf("armor line %q", l)
		}
	}
	blob, err := base64.StdEncoding.DecodeString(strings.Join(lines[1:len(lines)-1], ""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(blob, []byte(sigMagic)) {
		t.Fatalf("blob lacks the magic")
	}
	r := reader{b: blob[len(sigMagic):]}
	if v := r.u32(); v != sigVersion {
		t.Fatalf("version %d", v)
	}
	pubBlob = r.str()
	namespace = string(r.str())
	if reserved := r.str(); len(reserved) != 0 {
		t.Fatalf("reserved = %q", reserved)
	}
	hash = string(r.str())
	sigBlob := r.str()
	if r.err || len(r.b) != 0 {
		t.Fatalf("malformed blob")
	}
	sr := reader{b: sigBlob}
	sigType = string(sr.str())
	sig = sr.str()
	if sr.err || len(sr.b) != 0 {
		t.Fatalf("malformed signature")
	}
	return pubBlob, namespace, hash, sigType, sig
}

func TestSign(t *testing.T) {
	t.Parallel()
	s, err := ParsePrivateKey(marshalKey(t, keyOpts{comment: "touchmark@example.com"}))
	if err != nil {
		t.Fatal(err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	wantPub := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(putString(putString(nil, []byte(keyType)), pub))
	if got := s.PublicKey(); got != wantPub {
		t.Errorf("PublicKey() = %q, want %q", got, wantPub)
	}
	message := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n\ncommit message\n")
	armored, err := s.Sign(Namespace, message)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(armored, "\n") {
		t.Errorf("signature ends with a newline")
	}
	pubBlob, ns, hash, sigType, sig := decodeArmor(t, armored)
	if ns != Namespace || hash != "sha512" || sigType != keyType || !bytes.Equal(pubBlob, putString(putString(nil, []byte(keyType)), pub)) {
		t.Errorf("fields: namespace %q, hash %q, type %q", ns, hash, sigType)
	}
	h := sha512.Sum512(message)
	signed := append([]byte(sigMagic), putString(nil, []byte(Namespace))...)
	signed = putString(signed, nil)
	signed = putString(signed, []byte("sha512"))
	signed = putString(signed, h[:])
	if !ed25519.Verify(pub, signed, sig) {
		t.Error("the signature does not verify")
	}
	// Deterministic: same key and message, same signature.
	again, err := s.Sign(Namespace, message)
	if err != nil || again != armored {
		t.Errorf("Sign() again = %q, %v", again, err)
	}
	other, err := s.Sign(Namespace, append(message, '!'))
	if err != nil || other == armored {
		t.Errorf("Sign(other message) = %q, %v", other, err)
	}
	if _, err := s.Sign("", message); err == nil {
		t.Error("Sign(empty namespace) succeeded")
	}
	var nilSigner *Signer
	if _, err := nilSigner.Sign(Namespace, message); err == nil {
		t.Error("nil Signer signed")
	}
	if nilSigner.PublicKey() != "" {
		t.Error("nil Signer has a public key")
	}
}

func TestArmor(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 52, 53, 54, 105, 300} {
		blob := bytes.Repeat([]byte{0xab}, n)
		a := armor(blob)
		lines := strings.Split(a, "\n")
		if lines[0] != armorBegin || lines[len(lines)-1] != armorEnd {
			t.Fatalf("armor(%d) = %q", n, a)
		}
		body := lines[1 : len(lines)-1]
		for i, l := range body {
			if len(l) > armorWidth || (i < len(body)-1 && len(l) != armorWidth) {
				t.Errorf("armor(%d): line %d has %d characters", n, i, len(l))
			}
		}
		if got, err := base64.StdEncoding.DecodeString(strings.Join(body, "")); err != nil || !bytes.Equal(got, blob) {
			t.Errorf("armor(%d) does not decode: %v", n, err)
		}
	}
}

func TestParsePrivateKeyRejects(t *testing.T) {
	t.Parallel()
	good := marshalKey(t, keyOpts{})
	if _, err := ParsePrivateKey(good); err != nil {
		t.Fatalf("ParsePrivateKey(good) = %v", err)
	}
	if _, err := ParsePrivateKey(append(append([]byte("\n  "), good...), "\n\n"...)); err != nil {
		t.Errorf("ParsePrivateKey(surrounded by whitespace) = %v", err)
	}
	block, _ := pem.Decode(good)
	withHeaders := pem.EncodeToMemory(&pem.Block{Type: pemType, Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: block.Bytes})
	cases := map[string]struct {
		data []byte
		in   string // the error names this
	}{
		"empty":         {nil, "PEM"},
		"not pem":       {[]byte("ssh-ed25519 AAAA key"), "PEM"},
		"junk before":   {append([]byte("junk\n"), good...), "PEM"},
		"rsa pem":       {pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{1}}), "RSA PRIVATE KEY"},
		"headers":       {withHeaders, "headers"},
		"two blocks":    {append(cloneBytes(good), good...), "after"},
		"trailing text": {append(cloneBytes(good), "extra"...), "after"},
		"bad magic":     {pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: []byte("openssh-key-v2\x00")}), "openssh-key-v1"},
		"truncated":     {pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: block.Bytes[:len(block.Bytes)-9]}), "malformed"},
		"encrypted":     {marshalKey(t, keyOpts{cipher: "aes256-ctr", kdf: "bcrypt", kdfOptions: "salt+rounds"}), "encrypted"},
		"kdf only":      {marshalKey(t, keyOpts{kdf: "bcrypt"}), "encrypted"},
		"two keys":      {marshalKey(t, keyOpts{keys: 2}), "2 keys"},
		"check ints":    {marshalKey(t, keyOpts{checkDiffers: true}), "check integers"},
		"rsa":           {marshalKey(t, keyOpts{typ: "ssh-rsa"}), "ssh-rsa"},
		"public rsa":    {marshalKey(t, keyOpts{pubType: "ssh-rsa"}), "ssh-rsa"},
		"sk key":        {marshalKey(t, keyOpts{pubType: "sk-ssh-ed25519@openssh.com"}), "sk-ssh-ed25519"},
		"other public":  {marshalKey(t, keyOpts{otherPub: true}), "differs"},
		"bad padding":   {marshalKey(t, keyOpts{badPadding: true, comment: "x"}), "malformed"},
		"trailing data": {marshalKey(t, keyOpts{trailing: []byte{0, 0, 0, 0}}), "malformed"},
		"too large":     {bytes.Repeat([]byte("a"), maxKeyBytes+1), "too large"},
	}
	for name, c := range cases {
		s, err := ParsePrivateKey(c.data)
		if err == nil {
			t.Errorf("%s: ParsePrivateKey succeeded: %v", name, s.PublicKey())
			continue
		}
		if !strings.Contains(err.Error(), c.in) {
			t.Errorf("%s: error %q does not name %q", name, err, c.in)
		}
		if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(seed[:12])) {
			t.Errorf("%s: the error leaks key material: %v", name, err)
		}
	}
}

func cloneBytes(b []byte) []byte { return append([]byte(nil), b...) }

// FuzzParsePrivateKey: any input is parsed or refused, never a panic; a
// key that parses signs.
func FuzzParsePrivateKey(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte("-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := ParsePrivateKey(data)
		if err != nil {
			return
		}
		if _, err := s.Sign(Namespace, data); err != nil || !strings.HasPrefix(s.PublicKey(), "ssh-ed25519 ") {
			t.Fatalf("a parsed key does not sign: %v", err)
		}
	})
}

// FuzzParseKey fuzzes the openssh-key-v1 structure under a valid armor.
func FuzzParseKey(f *testing.F) {
	block, _ := pem.Decode(marshalKey(f, keyOpts{comment: "c"}))
	f.Add(block.Bytes)
	f.Add([]byte(keyMagic))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := parseKey(b)
		if err == nil && s.PublicKey() == "" {
			t.Fatal("a parsed key has no public key")
		}
	})
}

// sshKeygen returns the path of ssh-keygen, skipping without it.
func sshKeygen(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skipf("ssh-keygen not found: %v", err)
	}
	return bin
}

// run runs ssh-keygen with stdin and returns stdout.
func run(t *testing.T, bin string, stdin []byte, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("ssh-keygen %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// TestSSHKeygen checks keys and signatures against OpenSSH: a key
// ssh-keygen made parses, the public key matches its .pub file, a
// signature verifies with `ssh-keygen -Y verify`, and it is byte for byte
// the one `ssh-keygen -Y sign` makes.
func TestSSHKeygen(t *testing.T) {
	t.Parallel()
	bin := sshKeygen(t)
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if _, err := run(t, bin, nil, "-q", "-t", "ed25519", "-N", "", "-C", "touchmark test key", "-f", key); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParsePrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	pubFile, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(pubFile))
	if len(f) < 2 || s.PublicKey() != f[0]+" "+f[1] {
		t.Errorf("PublicKey() = %q, .pub holds %q", s.PublicKey(), pubFile)
	}

	message := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor a <a@example.com> 1 +0000\n\nsigned by touchmark\n")
	armored, err := s.Sign(Namespace, message)
	if err != nil {
		t.Fatal(err)
	}
	sigFile := filepath.Join(dir, "msg.sig")
	if err := os.WriteFile(sigFile, []byte(armored), 0o644); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(allowed, []byte("bot@example.com "+s.PublicKey()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, bin, message, "-Y", "verify", "-n", Namespace, "-f", allowed, "-I", "bot@example.com", "-s", sigFile); err != nil {
		t.Errorf("ssh-keygen -Y verify rejected the signature: %v", err)
	}
	// Another namespace or message fails.
	if _, err := run(t, bin, message, "-Y", "verify", "-n", "file", "-f", allowed, "-I", "bot@example.com", "-s", sigFile); err == nil {
		t.Error("ssh-keygen -Y verify accepted the signature in another namespace")
	}
	if _, err := run(t, bin, append(message, 'x'), "-Y", "verify", "-n", Namespace, "-f", allowed, "-I", "bot@example.com", "-s", sigFile); err == nil {
		t.Error("ssh-keygen -Y verify accepted another message")
	}
	// ssh-keygen signs the same bytes.
	theirs, err := run(t, bin, message, "-Y", "sign", "-n", Namespace, "-f", key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSuffix(string(theirs), "\n") != armored {
		t.Errorf("ssh-keygen -Y sign =\n%s\nours =\n%s", theirs, armored)
	}

	// Keys ParsePrivateKey refuses.
	for name, args := range map[string][]string{
		"passphrase": {"-t", "ed25519", "-N", "correct horse"},
		"ecdsa":      {"-t", "ecdsa", "-N", ""},
	} {
		k := filepath.Join(dir, name)
		if _, err := run(t, bin, nil, append([]string{"-q", "-C", name, "-f", k}, args...)...); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(k)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParsePrivateKey(data); err == nil {
			t.Errorf("ParsePrivateKey(%s key) succeeded", name)
		}
	}
}
