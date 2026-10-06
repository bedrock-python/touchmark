package provenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// manifestJSON has the fields and tags of Manifest without its methods, so
// encoding it does not recurse into MarshalJSON.
type manifestJSON Manifest

// MarshalJSON encodes the manifest deterministically: "paths" is always an
// object (never null), map keys are sorted (encoding/json sorts them),
// versions keep their recorded order, and '<', '>' and '&' are not escaped.
// Equal manifests encode to identical bytes.
func (m *Manifest) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	v := manifestJSON(*m)
	if v.Paths == nil {
		v.Paths = map[string]map[string][]Version{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// WriteJSON writes the manifest as JSON indented by two spaces, followed by
// a newline: the output of `touchmark manifest`.
func (m *Manifest) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// ReadManifestJSON parses a manifest written by MarshalJSON or WriteJSON.
//
// It is strict: unknown fields, trailing data, a version other than
// ManifestVersion, a hub_commit that is not a full object id, invalid pack
// names, paths that fail pathx.Validate, a path or pack without versions,
// malformed or duplicate version ids and negative sizes are errors. Version
// ids must have the length of hub_commit (one hash algorithm). A missing or
// null "paths" reads as an empty manifest.
func ReadManifestJSON(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var v manifestJSON
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("read manifest: unexpected data after the manifest")
	}
	m := Manifest(v)
	if m.Paths == nil {
		m.Paths = map[string]map[string][]Version{}
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return &m, nil
}

// validate checks the invariants ReadManifestJSON promises. It visits paths
// and packs in sorted order, so the first error is deterministic.
func (m *Manifest) validate() error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("unsupported version %d (want %d)", m.Version, ManifestVersion)
	}
	if !isOID(m.HubCommit) {
		return fmt.Errorf("hub_commit %.80q is not a full object id", m.HubCommit)
	}
	for _, p := range sortedKeys(m.Paths) {
		if err := pathx.Validate(p); err != nil {
			return err
		}
		byPack := m.Paths[p]
		if len(byPack) == 0 {
			return fmt.Errorf("path %q lists no packs", p)
		}
		for _, pack := range sortedKeys(byPack) {
			if err := checkPackName(pack); err != nil {
				return fmt.Errorf("path %q: %w", p, err)
			}
			if err := m.validateVersions(byPack[pack]); err != nil {
				return fmt.Errorf("path %q, pack %q: %w", p, pack, err)
			}
		}
	}
	return nil
}

// validateVersions checks one version list: not empty, well-formed ids of
// the hub's hash length, no duplicates, no negative sizes.
func (m *Manifest) validateVersions(vs []Version) error {
	if len(vs) == 0 {
		return errors.New("no versions")
	}
	seen := make(map[string]bool, len(vs))
	for _, v := range vs {
		switch {
		case !isOID(v.OID) || len(v.OID) != len(m.HubCommit):
			return fmt.Errorf("version id %.80q is not a full object id like hub_commit", v.OID)
		case seen[v.OID]:
			return fmt.Errorf("duplicate version %s", v.OID)
		case v.Size < 0:
			return fmt.Errorf("version %s has negative size %d", v.OID, v.Size)
		}
		seen[v.OID] = true
	}
	return nil
}
