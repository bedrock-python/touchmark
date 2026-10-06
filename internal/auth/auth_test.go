package auth

import (
	"bytes"
	"encoding/pem"
	"slices"
	"strings"
	"testing"
)

// envOf returns a getenv over name, value pairs.
func envOf(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(name string) string { return m[name] }
}

// testKey returns a PEM block of type typ with made-up content: auth never
// parses the key itself, and a real key in the repository would trip
// secret scanners.
func testKey(typ string, seed byte) string {
	der := make([]byte, 1200)
	for i := range der {
		der[i] = byte(i*31+i/251) ^ seed
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

var (
	rsaKey = testKey("RSA PRIVATE KEY", 1)
	sshKey = testKey("OPENSSH PRIVATE KEY", 7)
)

// oneLine returns key the way CI variables often store it: one line with
// literal \n sequences.
func oneLine(key string) string {
	return strings.ReplaceAll(strings.TrimSuffix(key, "\n"), "\n", `\n`)
}

const (
	tokenA = "ghs_a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6q7R8"
	tokenB = "glpat-Zz9Yy8Xx7Ww6Vv5Uu4Tt3"
)

func TestFromEnv(t *testing.T) {
	trimmedKey := strings.TrimSpace(rsaKey)
	tests := []struct {
		name   string
		prefix string
		short  bool
		role   Role
		env    func(string) string
		want   Credential
		ok     bool
		err    string // substring of the error; "" for none
	}{
		{
			name: "nothing set", prefix: "TOUCHMARK_GH_", short: true, role: Read,
			env: envOf("TOUCHMARK_GH_WRITE_TOKEN", tokenA, "TOUCHMARK_WRITE_TOKEN", tokenB),
		},
		{
			name: "prefixed token", prefix: "TOUCHMARK_GH_", role: Read,
			env:  envOf("TOUCHMARK_GH_READ_TOKEN", "  "+tokenA+"\r\n"),
			want: Credential{Kind: Token, Token: tokenA}, ok: true,
		},
		{
			name: "write token", prefix: "TOUCHMARK_CORP_GL_", role: Write,
			env:  envOf("TOUCHMARK_CORP_GL_READ_TOKEN", tokenA, "TOUCHMARK_CORP_GL_WRITE_TOKEN", tokenB),
			want: Credential{Kind: Token, Token: tokenB}, ok: true,
		},
		{
			name: "short token accepted with short", prefix: "TOUCHMARK_GH_", short: true, role: Read,
			env:  envOf("TOUCHMARK_READ_TOKEN", tokenB),
			want: Credential{Kind: Token, Token: tokenB}, ok: true,
		},
		{
			name: "short token ignored without short", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_READ_TOKEN", tokenB),
		},
		{
			name: "prefixed token wins over the short one", prefix: "TOUCHMARK_GH_", short: true, role: Read,
			env:  envOf("TOUCHMARK_GH_READ_TOKEN", tokenA, "TOUCHMARK_READ_TOKEN", tokenB),
			want: Credential{Kind: Token, Token: tokenA}, ok: true,
		},
		{
			name: "prefixed app wins over a short token, without mixing", prefix: "TOUCHMARK_GH_", short: true, role: Write,
			env: envOf("TOUCHMARK_GH_WRITE_APP_ID", "12345", "TOUCHMARK_GH_WRITE_APP_KEY", rsaKey,
				"TOUCHMARK_WRITE_TOKEN", tokenB),
			want: Credential{Kind: App, AppID: "12345", AppKey: []byte(trimmedKey)}, ok: true,
		},
		{
			name: "a partial prefixed set still wins", prefix: "TOUCHMARK_GH_", short: true, role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "12345",
				"TOUCHMARK_READ_APP_ID", "12345", "TOUCHMARK_READ_APP_KEY", rsaKey),
			err: "TOUCHMARK_GH_READ_APP_ID is set without TOUCHMARK_GH_READ_APP_KEY",
		},
		{
			name: "a blank prefixed variable is unset", prefix: "TOUCHMARK_GH_", short: true, role: Read,
			env:  envOf("TOUCHMARK_GH_READ_TOKEN", " \t\n", "TOUCHMARK_READ_TOKEN", tokenB),
			want: Credential{Kind: Token, Token: tokenB}, ok: true,
		},
		{
			name: "short prefix itself", prefix: "TOUCHMARK_", short: true, role: Read,
			env:  envOf("TOUCHMARK_READ_TOKEN", tokenB),
			want: Credential{Kind: Token, Token: tokenB}, ok: true,
		},
		{
			name: "app", prefix: "TOUCHMARK_GH_", role: Read,
			env:  envOf("TOUCHMARK_GH_READ_APP_ID", " 712345 \n", "TOUCHMARK_GH_READ_APP_KEY", "\n"+rsaKey+"\n\n"),
			want: Credential{Kind: App, AppID: "712345", AppKey: []byte(trimmedKey)}, ok: true,
		},
		{
			name: "app key with literal newlines", prefix: "TOUCHMARK_GH_", role: Read,
			env:  envOf("TOUCHMARK_GH_READ_APP_ID", "712345", "TOUCHMARK_GH_READ_APP_KEY", oneLine(rsaKey)),
			want: Credential{Kind: App, AppID: "712345", AppKey: []byte(trimmedKey)}, ok: true,
		},
		{
			name: "app key with literal CRLF", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "712345",
				"TOUCHMARK_GH_READ_APP_KEY", strings.ReplaceAll(oneLine(rsaKey), `\n`, `\r\n`)),
			want: Credential{Kind: App, AppID: "712345", AppKey: []byte(trimmedKey)}, ok: true,
		},
		{
			name: "app key with CRLF line endings", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "712345",
				"TOUCHMARK_GH_READ_APP_KEY", strings.ReplaceAll(rsaKey, "\n", "\r\n")),
			want: Credential{Kind: App, AppID: "712345", AppKey: []byte(strings.TrimSpace(strings.ReplaceAll(rsaKey, "\n", "\r\n")))}, ok: true,
		},
		{
			name: "PKCS#8 key", prefix: "TOUCHMARK_GH_", role: Read,
			env:  envOf("TOUCHMARK_GH_READ_APP_ID", "1", "TOUCHMARK_GH_READ_APP_KEY", testKey("PRIVATE KEY", 3)),
			want: Credential{Kind: App, AppID: "1", AppKey: []byte(strings.TrimSpace(testKey("PRIVATE KEY", 3)))}, ok: true,
		},
		{
			name: "token and app id", prefix: "TOUCHMARK_GH_", role: Write,
			env: envOf("TOUCHMARK_GH_WRITE_TOKEN", tokenA, "TOUCHMARK_GH_WRITE_APP_ID", "12345"),
			err: "TOUCHMARK_GH_WRITE_TOKEN and TOUCHMARK_GH_WRITE_APP_ID are both set",
		},
		{
			name: "token and app key", prefix: "TOUCHMARK_GH_", role: Write,
			env: envOf("TOUCHMARK_GH_WRITE_TOKEN", tokenA, "TOUCHMARK_GH_WRITE_APP_KEY", rsaKey),
			err: "TOUCHMARK_GH_WRITE_TOKEN and TOUCHMARK_GH_WRITE_APP_KEY are both set",
		},
		{
			name: "short token and short app", prefix: "TOUCHMARK_GH_", short: true, role: Read,
			env: envOf("TOUCHMARK_READ_TOKEN", tokenA, "TOUCHMARK_READ_APP_ID", "1", "TOUCHMARK_READ_APP_KEY", rsaKey),
			err: "TOUCHMARK_READ_TOKEN and TOUCHMARK_READ_APP_ID are both set",
		},
		{
			name: "app id without key", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "12345"),
			err: "TOUCHMARK_GH_READ_APP_ID is set without TOUCHMARK_GH_READ_APP_KEY",
		},
		{
			name: "app key without id", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_KEY", rsaKey),
			err: "TOUCHMARK_GH_READ_APP_KEY is set without TOUCHMARK_GH_READ_APP_ID",
		},
		{
			name: "client id instead of app id", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "Iv23liAbCdEf", "TOUCHMARK_GH_READ_APP_KEY", rsaKey),
			err: "TOUCHMARK_GH_READ_APP_ID must be a positive integer",
		},
		{
			name: "zero app id", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "0", "TOUCHMARK_GH_READ_APP_KEY", rsaKey),
			err: "must be a positive integer",
		},
		{
			name: "negative app id", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "-5", "TOUCHMARK_GH_READ_APP_KEY", rsaKey),
			err: "must be a positive integer",
		},
		{
			name: "app id with a leading zero", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "0123", "TOUCHMARK_GH_READ_APP_KEY", rsaKey),
			err: "must be a positive integer",
		},
		{
			name: "app id too long", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "1234567890123456789", "TOUCHMARK_GH_READ_APP_KEY", rsaKey),
			err: "at most 18 digits",
		},
		{
			name: "key that is not PEM", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "1", "TOUCHMARK_GH_READ_APP_KEY", "MIIEowIBAAKCAQEAsecretsecretsecret"),
			err: "TOUCHMARK_GH_READ_APP_KEY is not a PEM private key",
		},
		{
			name: "a public key", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "1", "TOUCHMARK_GH_READ_APP_KEY", testKey("PUBLIC KEY", 5)),
			err: "is not a PEM private key",
		},
		{
			name: "a certificate", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "1", "TOUCHMARK_GH_READ_APP_KEY", testKey("CERTIFICATE", 5)),
			err: "is not a PEM private key",
		},
		{
			name: "text before the key", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "1", "TOUCHMARK_GH_READ_APP_KEY", "key:\n"+rsaKey),
			err: "is not a PEM private key",
		},
		{
			name: "a truncated key", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "1", "TOUCHMARK_GH_READ_APP_KEY", rsaKey[:len(rsaKey)/2]),
			err: "is not a PEM private key",
		},
		{
			name: "two blocks", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_APP_ID", "1", "TOUCHMARK_GH_READ_APP_KEY", rsaKey+testKey("CERTIFICATE", 9)),
			err: "TOUCHMARK_GH_READ_APP_KEY holds more than the private key",
		},
		{
			name: "token with a space", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_TOKEN", "Bearer "+tokenA),
			err: "TOUCHMARK_GH_READ_TOKEN holds a space",
		},
		{
			name: "token with a newline inside", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_TOKEN", tokenA+"\r\nX-Injected: 1"),
			err: "TOUCHMARK_GH_READ_TOKEN holds a space, a control character",
		},
		{
			name: "token outside ASCII", prefix: "TOUCHMARK_GH_", role: Read,
			env: envOf("TOUCHMARK_GH_READ_TOKEN", tokenA+"é"),
			err: "outside ASCII",
		},
		{
			name: "unknown role", prefix: "TOUCHMARK_GH_", role: "ADMIN",
			env: envOf("TOUCHMARK_GH_ADMIN_TOKEN", tokenA),
			err: `unknown role "ADMIN"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, ok, err := FromEnv(tt.prefix, tt.short, tt.role, tt.env)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("FromEnv = %+v, %v, %v; want an error with %q", c, ok, err, tt.err)
				}
				if ok || c.Kind != 0 {
					t.Errorf("an error and a credential: %+v, %v", c, ok)
				}
				assertNoSecret(t, err.Error())
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok != tt.ok || c.Kind != tt.want.Kind || c.Token != tt.want.Token || c.AppID != tt.want.AppID || !bytes.Equal(c.AppKey, tt.want.AppKey) {
				t.Fatalf("FromEnv = %+v, %v; want %+v, %v", c, ok, tt.want, tt.ok)
			}
			if !ok {
				return
			}
			if len(c.Secrets()) == 0 {
				t.Error("a valid credential without secrets")
			}
			if c.Kind == App {
				if block, rest := pem.Decode(c.AppKey); block == nil || len(bytes.TrimSpace(rest)) > 0 {
					t.Errorf("the key does not decode as one PEM block: %q", c.AppKey)
				}
			}
		})
	}
}

// assertNoSecret fails when msg quotes any test secret.
func assertNoSecret(t *testing.T, msg string) {
	t.Helper()
	for _, s := range []string{tokenA[4:20], tokenB[6:20], "MIIEow", "secretsecret", "Iv23li", "712345", "BEGIN"} {
		if strings.Contains(msg, s) {
			t.Errorf("the error quotes a value (%q): %s", s, msg)
		}
	}
	for _, line := range strings.Split(rsaKey, "\n") {
		if len(line) > 16 && strings.Contains(msg, line) {
			t.Errorf("the error quotes the key: %s", msg)
		}
	}
}

func TestFromEnvArguments(t *testing.T) {
	env := envOf("TOUCHMARK_READ_TOKEN", tokenA, "READ_TOKEN", tokenA, "X_READ_TOKEN", tokenA)
	for _, prefix := range []string{"", "X_", "TOUCHMARK", "TOUCHMARK_gh_", "TOUCHMARK_GH", "TOUCHMARK__", "TOUCHMARK_G-H_", "OTHER_GH_"} {
		if _, ok, err := FromEnv(prefix, true, Read, env); err == nil || ok {
			t.Errorf("FromEnv(%q) = %v, %v; want an error", prefix, ok, err)
		}
		if _, ok, err := SigningKey(prefix, true, env); err == nil || ok {
			t.Errorf("SigningKey(%q) = %v, %v; want an error", prefix, ok, err)
		}
	}
	for _, prefix := range []string{"TOUCHMARK_", "TOUCHMARK_GH_", "TOUCHMARK_CORP_GL_", "TOUCHMARK_A1_"} {
		if _, _, err := FromEnv(prefix, true, Read, env); err != nil {
			t.Errorf("FromEnv(%q): %v", prefix, err)
		}
	}
}

// TestFromEnvProcessEnv: a nil getenv reads the process environment.
func TestFromEnvProcessEnv(t *testing.T) {
	t.Setenv("TOUCHMARK_ZZTEST_READ_TOKEN", tokenA)
	c, ok, err := FromEnv("TOUCHMARK_ZZTEST_", false, Read, nil)
	if err != nil || !ok || c.Token != tokenA {
		t.Errorf("FromEnv = %+v, %v, %v", c, ok, err)
	}
	t.Setenv("TOUCHMARK_ZZTEST_SIGNING_KEY", sshKey)
	if _, ok, err := SigningKey("TOUCHMARK_ZZTEST_", false, nil); err != nil || !ok {
		t.Errorf("SigningKey = %v, %v", ok, err)
	}
}

func TestSigningKey(t *testing.T) {
	trimmed := []byte(strings.TrimSpace(sshKey))
	tests := []struct {
		name  string
		short bool
		env   func(string) string
		want  []byte
		ok    bool
		err   string
	}{
		{name: "unset", short: true, env: envOf("TOUCHMARK_GH_READ_TOKEN", tokenA)},
		{name: "prefixed", env: envOf("TOUCHMARK_GH_SIGNING_KEY", sshKey), want: trimmed, ok: true},
		{name: "short", short: true, env: envOf("TOUCHMARK_SIGNING_KEY", "  "+sshKey), want: trimmed, ok: true},
		{name: "short ignored without short", env: envOf("TOUCHMARK_SIGNING_KEY", sshKey)},
		{
			name: "prefixed wins", short: true,
			env:  envOf("TOUCHMARK_GH_SIGNING_KEY", sshKey, "TOUCHMARK_SIGNING_KEY", "junk"),
			want: trimmed, ok: true,
		},
		{name: "literal newlines", env: envOf("TOUCHMARK_GH_SIGNING_KEY", oneLine(sshKey)), want: trimmed, ok: true},
		{name: "not PEM", env: envOf("TOUCHMARK_GH_SIGNING_KEY", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPublicKey"), err: "TOUCHMARK_GH_SIGNING_KEY is not a PEM private key"},
		{name: "short not PEM", short: true, env: envOf("TOUCHMARK_SIGNING_KEY", "junk"), err: "TOUCHMARK_SIGNING_KEY is not a PEM private key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, ok, err := SigningKey("TOUCHMARK_GH_", tt.short, tt.env)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) || ok || key != nil {
					t.Fatalf("SigningKey = %q, %v, %v; want an error with %q", key, ok, err, tt.err)
				}
				if strings.Contains(err.Error(), "AAAAC3") || strings.Contains(err.Error(), "junk") {
					t.Errorf("the error quotes the value: %v", err)
				}
				return
			}
			if err != nil || ok != tt.ok || !bytes.Equal(key, tt.want) {
				t.Fatalf("SigningKey = %q, %v, %v; want %q, %v", key, ok, err, tt.want, tt.ok)
			}
		})
	}
}

func TestNames(t *testing.T) {
	want := []string{
		"TOUCHMARK_GH_READ_TOKEN", "TOUCHMARK_GH_READ_APP_ID", "TOUCHMARK_GH_READ_APP_KEY",
		"TOUCHMARK_GH_WRITE_TOKEN", "TOUCHMARK_GH_WRITE_APP_ID", "TOUCHMARK_GH_WRITE_APP_KEY",
		"TOUCHMARK_GH_SIGNING_KEY",
	}
	if got := Names("TOUCHMARK_GH_", false); !slices.Equal(got, want) {
		t.Errorf("Names(prefixed) = %q\nwant %q", got, want)
	}
	short := []string{
		"TOUCHMARK_READ_TOKEN", "TOUCHMARK_READ_APP_ID", "TOUCHMARK_READ_APP_KEY",
		"TOUCHMARK_WRITE_TOKEN", "TOUCHMARK_WRITE_APP_ID", "TOUCHMARK_WRITE_APP_KEY",
		"TOUCHMARK_SIGNING_KEY",
	}
	if got := Names("TOUCHMARK_GH_", true); !slices.Equal(got, append(slices.Clone(want), short...)) {
		t.Errorf("Names(prefixed, short) = %q", got)
	}
	if got := Names("TOUCHMARK_", true); !slices.Equal(got, short) {
		t.Errorf("Names(short prefix) = %q, want %q", got, short)
	}
}

// TestNamesCovers: every variable FromEnv and SigningKey read, in every
// combination of set variables, is in Names.
func TestNamesCovers(t *testing.T) {
	suffixes := []string{"READ_TOKEN", "READ_APP_ID", "READ_APP_KEY", "WRITE_TOKEN", "WRITE_APP_ID", "WRITE_APP_KEY", "SIGNING_KEY"}
	valueFor := func(name string) string {
		switch {
		case strings.HasSuffix(name, "_TOKEN"):
			return tokenA
		case strings.HasSuffix(name, "_APP_ID"):
			return "12"
		case strings.HasSuffix(name, "SIGNING_KEY"):
			return sshKey
		}
		return rsaKey
	}
	for _, prefix := range []string{"TOUCHMARK_GH_", "TOUCHMARK_"} {
		for _, short := range []bool{false, true} {
			names := Names(prefix, short)
			// Every combination of up to two set variables, under the
			// prefix or the short form.
			var all []string
			for _, s := range suffixes {
				all = append(all, prefix+s, "TOUCHMARK_"+s)
			}
			for i := -1; i < len(all); i++ {
				for j := i; j < len(all); j++ {
					set := map[string]string{}
					for _, k := range []int{i, j} {
						if k >= 0 {
							set[all[k]] = valueFor(all[k])
						}
					}
					var read []string
					getenv := func(name string) string {
						read = append(read, name)
						return set[name]
					}
					for _, role := range []Role{Read, Write} {
						_, _, _ = FromEnv(prefix, short, role, getenv)
					}
					_, _, _ = SigningKey(prefix, short, getenv)
					for _, name := range read {
						if !slices.Contains(names, name) {
							t.Fatalf("%s (short %v) read %s, which Names lacks", prefix, short, name)
						}
					}
				}
			}
		}
	}
}

func TestSecrets(t *testing.T) {
	if s := (Credential{}).Secrets(); s != nil {
		t.Errorf("zero credential: %q", s)
	}
	if s := (Credential{Kind: Token}).Secrets(); s != nil {
		t.Errorf("empty token: %q", s)
	}
	if s := (Credential{Kind: App, AppID: "1", AppKey: []byte(" \n")}).Secrets(); s != nil {
		t.Errorf("blank key: %q", s)
	}
	if s := (Credential{Kind: Token, Token: tokenA}).Secrets(); !slices.Equal(s, []string{tokenA}) {
		t.Errorf("token: %q", s)
	}

	c, ok, err := FromEnv("TOUCHMARK_GH_", false, Write, envOf("TOUCHMARK_GH_WRITE_APP_ID", "712345", "TOUCHMARK_GH_WRITE_APP_KEY", oneLine(rsaKey)))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	secrets := c.Secrets()
	if len(secrets) < 2 || secrets[0] != string(c.AppKey) {
		t.Fatalf("app secrets = %q, want the key first", secrets)
	}
	lines := strings.Split(strings.TrimSpace(rsaKey), "\n")
	body := lines[1 : len(lines)-1]
	for _, line := range body {
		if len(line) > 16 && !slices.Contains(secrets, line) {
			t.Errorf("line %q of the key is not a secret", line)
		}
	}
	if len(secrets) != 1+len(body) {
		t.Errorf("%d secrets, want the key and its %d body lines", len(secrets), len(body))
	}
	for i, s := range secrets {
		if (i > 0 && strings.HasPrefix(s, "-----")) || s == "" || strings.Contains(s, "712345") {
			t.Errorf("secret %q is armor, empty or the app id", s)
		}
		if !strings.Contains(string(c.AppKey), s) {
			t.Errorf("secret %q is not part of the key", s)
		}
	}
	if sorted := slices.Sorted(slices.Values(secrets)); len(slices.Compact(sorted)) != len(secrets) {
		t.Errorf("duplicate secrets: %q", secrets)
	}
	// A body line that repeats, not next to itself, is masked once.
	repeat := strings.Repeat("B", 64)
	twice := Credential{Kind: App, AppID: "1", AppKey: []byte("-----BEGIN PRIVATE KEY-----\n" + repeat + "\n" +
		strings.Repeat("C", 64) + "\n" + repeat + "\n-----END PRIVATE KEY-----")}
	got := twice.Secrets()
	if sorted := slices.Sorted(slices.Values(got)); len(slices.Compact(sorted)) != len(got) || len(got) != 3 {
		t.Errorf("a repeated line: secrets %q, want the key and two distinct lines", got)
	}
	// A key with CRLF line endings yields lines without the CR.
	crlf := Credential{Kind: App, AppID: "1", AppKey: []byte(strings.ReplaceAll(rsaKey, "\n", "\r\n"))}
	for _, s := range crlf.Secrets()[1:] {
		if strings.ContainsAny(s, "\r\n") {
			t.Errorf("CRLF key: secret %q holds a line ending", s)
		}
	}
	// A line of 16 bytes or less is not masked on its own.
	short := Credential{Kind: App, AppID: "1", AppKey: []byte("-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("A", 64) + "\nQUJD\n-----END PRIVATE KEY-----")}
	if got := short.Secrets(); len(got) != 2 || got[1] != strings.Repeat("A", 64) {
		t.Errorf("short last line: %q", got)
	}
}
