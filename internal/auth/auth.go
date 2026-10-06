// Package auth reads provider credentials from the environment (see
// docs/reference/environment.md). Secrets never live in config files.
//
// Variable names, with <P> the provider's EnvPrefix ("TOUCHMARK_GH_" for
// the provider gh):
//
//	<P>READ_TOKEN   | <P>READ_APP_ID  + <P>READ_APP_KEY
//	<P>WRITE_TOKEN  | <P>WRITE_APP_ID + <P>WRITE_APP_KEY
//	<P>SIGNING_KEY  (ssh ed25519 private key, optional)
//
// A hub with one provider also accepts the short names, with <P> =
// "TOUCHMARK_" (TOUCHMARK_READ_TOKEN, …); the prefixed names win.
//
// GitHub App JWTs and installation tokens are the GitHub driver's
// (internal/platform/github); this package only reads what the environment
// holds. Error messages name variables and never
// quote their values.
package auth

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Role is read (plan) or write (distribute, doctor).
type Role string

const (
	Read  Role = "READ"
	Write Role = "WRITE"
)

// Kind of a credential.
type Kind uint8

const (
	// Token is a personal, project, group or service-account token.
	Token Kind = iota + 1
	// App is a GitHub App: an id and a PEM private key.
	App
)

// shortPrefix is the prefix of the short names.
const shortPrefix = "TOUCHMARK_"

// Name suffixes after <P><ROLE>, and the signing key's after <P>.
const (
	tokenSuffix  = "_TOKEN"
	appIDSuffix  = "_APP_ID"
	appKeySuffix = "_APP_KEY"
	signingName  = "SIGNING_KEY"
)

// maxAppIDDigits bounds an app id so that it fits an int64.
const maxAppIDDigits = 18

// minKeyLine is the length a line of a key must exceed to be a secret on
// its own: shorter lines are too common to mask.
const minKeyLine = 16

// Credential is one role's credential for one provider.
type Credential struct {
	Kind   Kind
	Token  string
	AppID  string
	AppKey []byte // PEM
}

// Secrets returns the strings a redactor must mask: the token, or the app
// key (and each of its lines longer than 16 bytes, which logs may print
// alone).
//
// The PEM armor lines ("-----BEGIN …", "-----END …") are not secret and
// are left out; the app id is not a secret either. Lines are trimmed of
// surrounding whitespace, including a CR. The result has no duplicates and
// is never empty for a credential FromEnv returned; it is nil for a zero
// Credential.
func (c Credential) Secrets() []string {
	switch c.Kind {
	case Token:
		if c.Token == "" {
			return nil
		}
		return []string{c.Token}
	case App:
		if len(bytes.TrimSpace(c.AppKey)) == 0 {
			return nil
		}
		out := []string{string(c.AppKey)}
		for _, line := range strings.Split(string(c.AppKey), "\n") {
			line = strings.TrimSpace(line)
			if len(line) > minKeyLine && !strings.HasPrefix(line, "-----") && !slices.Contains(out, line) {
				out = append(out, line)
			}
		}
		return out
	}
	return nil
}

// FromEnv reads the credential for role under prefix. With short set it
// also accepts "TOUCHMARK_" names when the prefixed ones are absent (a hub
// with one provider). It returns ok=false when nothing is set. It is an
// error to set both a token and an app, an app id without a key or the
// reverse, an app id that is not a positive integer, or a key that is not
// PEM. Values are trimmed of surrounding whitespace; a literal "\n" in a
// single-line key is turned into a newline (CI variables often store PEM
// that way).
//
// Names are read as one set: the prefixed set wins as soon as one of its
// variables is set, and the short set is then not read, so a token and an
// app never mix across the two forms. A variable holding only whitespace
// is unset. A token must be printable ASCII without spaces: it goes into
// HTTP headers. A key must be one PEM block, alone, whose type ends in
// "PRIVATE KEY"; its content is not parsed. An app id has at most 18
// digits. prefix must be "TOUCHMARK_" or an EnvPrefix, and role Read or
// Write. A nil getenv reads the process environment.
func FromEnv(prefix string, short bool, role Role, getenv func(string) string) (c Credential, ok bool, err error) {
	if err := checkPrefix(prefix); err != nil {
		return Credential{}, false, err
	}
	if role != Read && role != Write {
		return Credential{}, false, fmt.Errorf("auth: unknown role %q", role)
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	set := readSet(prefix, role, getenv)
	if !set.present() && short && prefix != shortPrefix {
		set = readSet(shortPrefix, role, getenv)
	}
	if !set.present() {
		return Credential{}, false, nil
	}
	c, err = set.credential()
	if err != nil {
		return Credential{}, false, err
	}
	return c, true, nil
}

// SigningKey reads <P>SIGNING_KEY (same prefix rules), ok=false if unset.
//
// The key is trimmed and a literal "\n" in a single-line value becomes a
// newline, as for app keys; it must be one PEM block whose type ends in
// "PRIVATE KEY" (an OpenSSH key is "OPENSSH PRIVATE KEY"). Whether it is
// ed25519 is checked where it is used.
func SigningKey(prefix string, short bool, getenv func(string) string) (key []byte, ok bool, err error) {
	if err := checkPrefix(prefix); err != nil {
		return nil, false, err
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	name := prefix + signingName
	value := strings.TrimSpace(getenv(name))
	if value == "" && short && prefix != shortPrefix {
		name = shortPrefix + signingName
		value = strings.TrimSpace(getenv(name))
	}
	if value == "" {
		return nil, false, nil
	}
	key, err = pemKey(name, value)
	if err != nil {
		return nil, false, err
	}
	return key, true, nil
}

// Names returns every variable name FromEnv and SigningKey may read for
// prefix, so the caller can unset them from the process environment once
// read, and no process the run starts inherits them.
//
// The order is fixed: READ_TOKEN, READ_APP_ID, READ_APP_KEY, the same for
// WRITE, then SIGNING_KEY, under prefix and then, with short, under
// "TOUCHMARK_". There are no duplicates.
func Names(prefix string, short bool) []string {
	names := namesUnder(prefix)
	if short && prefix != shortPrefix {
		names = append(names, namesUnder(shortPrefix)...)
	}
	return names
}

func namesUnder(prefix string) []string {
	var out []string
	for _, role := range []Role{Read, Write} {
		base := prefix + string(role)
		out = append(out, base+tokenSuffix, base+appIDSuffix, base+appKeySuffix)
	}
	return append(out, prefix+signingName)
}

// checkPrefix accepts "TOUCHMARK_" and "TOUCHMARK_<ID>_" with an id of
// uppercase letters, digits and underscores, as config.EnvPrefix makes
// them.
func checkPrefix(prefix string) error {
	rest, ok := strings.CutPrefix(prefix, shortPrefix)
	if !ok || (rest != "" && !strings.HasSuffix(rest, "_")) || strings.HasPrefix(rest, "_") {
		return fmt.Errorf("auth: invalid variable prefix %q", prefix)
	}
	for _, c := range rest {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return fmt.Errorf("auth: invalid variable prefix %q", prefix)
		}
	}
	return nil
}

// credentialVars is one role's set of variables under one prefix, read and
// trimmed.
type credentialVars struct {
	tokenName, appIDName, appKeyName string
	token, appID, appKey             string
}

func readSet(prefix string, role Role, getenv func(string) string) credentialVars {
	base := prefix + string(role)
	v := credentialVars{
		tokenName:  base + tokenSuffix,
		appIDName:  base + appIDSuffix,
		appKeyName: base + appKeySuffix,
	}
	v.token = strings.TrimSpace(getenv(v.tokenName))
	v.appID = strings.TrimSpace(getenv(v.appIDName))
	v.appKey = strings.TrimSpace(getenv(v.appKeyName))
	return v
}

func (v credentialVars) present() bool { return v.token != "" || v.appID != "" || v.appKey != "" }

// credential validates a present set.
func (v credentialVars) credential() (Credential, error) {
	switch {
	case v.token != "" && v.appID != "":
		return Credential{}, fmt.Errorf("%s and %s are both set: use a token or a GitHub App, not both", v.tokenName, v.appIDName)
	case v.token != "" && v.appKey != "":
		return Credential{}, fmt.Errorf("%s and %s are both set: use a token or a GitHub App, not both", v.tokenName, v.appKeyName)
	case v.token != "":
		if !printableToken(v.token) {
			return Credential{}, fmt.Errorf("%s holds a space, a control character or a character outside ASCII; set it to the token alone", v.tokenName)
		}
		return Credential{Kind: Token, Token: v.token}, nil
	case v.appKey == "":
		return Credential{}, fmt.Errorf("%s is set without %s", v.appIDName, v.appKeyName)
	case v.appID == "":
		return Credential{}, fmt.Errorf("%s is set without %s", v.appKeyName, v.appIDName)
	case !positiveInteger(v.appID):
		return Credential{}, fmt.Errorf("%s must be a positive integer of at most %d digits: the GitHub App's id", v.appIDName, maxAppIDDigits)
	}
	key, err := pemKey(v.appKeyName, v.appKey)
	if err != nil {
		return Credential{}, err
	}
	return Credential{Kind: App, AppID: v.appID, AppKey: key}, nil
}

// printableToken reports whether s is printable ASCII without spaces, as
// an HTTP header or a git config value may carry it.
func printableToken(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// positiveInteger reports whether s is decimal digits without a leading
// zero, at most maxAppIDDigits of them.
func positiveInteger(s string) bool {
	if s == "" || len(s) > maxAppIDDigits || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// pemKey turns the trimmed value of the variable name into a PEM private
// key: a single-line value has its literal "\n" (and "\r\n") sequences
// turned into newlines, and the result must be one PEM block with nothing
// around it, whose type ends in "PRIVATE KEY".
func pemKey(name, value string) ([]byte, error) {
	if !strings.Contains(value, "\n") {
		value = strings.ReplaceAll(value, `\r\n`, "\n")
		value = strings.ReplaceAll(value, `\n`, "\n")
	}
	errNotKey := errors.New(name + " is not a PEM private key")
	if !strings.HasPrefix(value, "-----BEGIN ") {
		return nil, errNotKey
	}
	block, rest := pem.Decode([]byte(value))
	if block == nil || !strings.HasSuffix(block.Type, "PRIVATE KEY") {
		return nil, errNotKey
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New(name + " holds more than the private key: set it to one PEM block")
	}
	return []byte(value), nil
}
