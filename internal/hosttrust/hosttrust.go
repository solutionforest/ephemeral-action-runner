// Package hosttrust snapshots host CA anchors into a deterministic, Linux
// container-consumable overlay. It intentionally models only CA anchors; host
// policy engines can carry additional constraints which a PEM bundle cannot.
package hosttrust

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	ModeDisabled = "disabled"
	ModeOverlay  = "overlay"

	ScopeSystem = "system"
	ScopeUser   = "user"

	canonicalSchemaVersion = 1
)

var ErrUnsupportedPlatform = errors.New("host trust collection is unsupported on this platform")

// Options controls one immutable host-trust resolution. ControllerHostOS is
// explicit so a containerized controller cannot accidentally claim it read the
// host OS's trust store; it must match the running collector unless FeedPath
// supplies an independently validated native-host snapshot.
type Options struct {
	Mode             string
	Scopes           []string
	FeedPath         string
	FeedSHA256       string
	ControllerHostOS string
	Now              func() time.Time
}

// Certificate is the deterministic PEM representation consumed by image
// builders. Name is stable across rotations of unrelated certificates.
type Certificate struct {
	Name   string `json:"name"`
	PEM    []byte `json:"pem"`
	SHA256 string `json:"sha256"`
}

// Source describes which host boundary contributed a snapshot. It is metadata,
// not a security assertion.
type Source struct {
	Scope string `json:"scope"`
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
}

// PolicyEntry records negative or constrained native policy that is deliberately
// excluded from the Linux PEM overlay.
type PolicyEntry struct {
	Scope  string `json:"scope"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
}

// Snapshot is frozen once per image generation. Certificates are sorted by
// SHA-256 and Generation is a content digest, not a wall-clock timestamp.
type Snapshot struct {
	Generation   string        `json:"generation"`
	HostOS       string        `json:"hostOS"`
	Scopes       []string      `json:"scopes"`
	Certificates []Certificate `json:"certificates"`
	Policy       []PolicyEntry `json:"policy,omitempty"`
	Sources      []Source      `json:"sources,omitempty"`
	CollectedAt  time.Time     `json:"collectedAt"`
}

// Enabled reports whether host-root overlay collection is requested.
func Enabled(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), ModeOverlay)
}

// Resolve captures either configured native roots or one freshness- and
// hash-checked external feed. Disabled mode is a zero, serializable snapshot
// and never reads host state.
func Resolve(ctx context.Context, options Options) (Snapshot, error) {
	now := time.Now().UTC()
	if options.Now != nil {
		now = options.Now().UTC()
	}
	hostOS := strings.ToLower(strings.TrimSpace(options.ControllerHostOS))
	if hostOS == "" {
		hostOS = runtime.GOOS
	}
	if !Enabled(options.Mode) {
		return Snapshot{HostOS: hostOS, CollectedAt: now}, nil
	}
	scopes, err := normalizeScopes(options.Scopes)
	if err != nil {
		return Snapshot{}, err
	}
	if options.FeedPath != "" {
		feed, err := ReadFeed(options.FeedPath, options.FeedSHA256, now)
		if err != nil {
			return Snapshot{}, err
		}
		if feed.HostOS != hostOS {
			return Snapshot{}, fmt.Errorf("host trust feed hostOS %q does not match requested controller host OS %q", feed.HostOS, hostOS)
		}
		if !sameStrings(feed.Scopes, scopes) {
			return Snapshot{}, fmt.Errorf("host trust feed scopes %v do not match requested scopes %v", feed.Scopes, scopes)
		}
		feed.CollectedAt = now
		return Canonicalize(feed)
	}
	if hostOS != runtime.GOOS {
		return Snapshot{}, fmt.Errorf("host trust collector runs on %s, not requested controller host OS %s", runtime.GOOS, hostOS)
	}
	snapshot, err := collectNative(ctx, scopes)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.HostOS = hostOS
	snapshot.Scopes = scopes
	snapshot.CollectedAt = now

	return Canonicalize(snapshot)
}

func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		scopes = []string{ScopeSystem}
	}
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		switch scope {
		case ScopeSystem, ScopeUser:
		default:
			return nil, fmt.Errorf("unsupported host trust scope %q", scope)
		}
		if _, exists := seen[scope]; exists {
			return nil, fmt.Errorf("duplicate host trust scope %q", scope)
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	sort.Strings(out)
	return out, nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func hasScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

// CertificatesFromBytes strictly accepts DER or a PEM sequence of CA
// certificates and returns a deterministic deduplicated set.
func CertificatesFromBytes(content []byte) ([]Certificate, error) {
	parsed, err := parseCertificates(content)
	if err != nil {
		return nil, err
	}
	for _, certificate := range parsed {
		if !certificate.IsCA {
			return nil, fmt.Errorf("certificate %q is not a CA", certificate.Subject.String())
		}
	}
	return certificatesFromParsed(parsed), nil
}

// CAAnchorsFromBytes parses a certificate sequence and intentionally ignores
// non-CA certificates. Native keychain commands can return mixed contents;
// externally supplied overrides and feeds should use CertificatesFromBytes.
func CAAnchorsFromBytes(content []byte) ([]Certificate, error) {
	parsed, err := parseCertificates(content)
	if err != nil {
		return nil, err
	}
	anchors := make([]*x509.Certificate, 0, len(parsed))
	for _, certificate := range parsed {
		if certificate.IsCA {
			anchors = append(anchors, certificate)
		}
	}
	return certificatesFromParsed(anchors), nil
}

func parseCertificates(content []byte) ([]*x509.Certificate, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("no certificates found")
	}
	var parsed []*x509.Certificate
	if bytes.HasPrefix(trimmed, []byte("-----BEGIN")) {
		rest := trimmed
		for len(rest) > 0 {
			block, next := pem.Decode(rest)
			if block == nil {
				return nil, fmt.Errorf("unexpected non-certificate content after PEM certificate")
			}
			if block.Type != "CERTIFICATE" {
				return nil, fmt.Errorf("PEM block type %q is not CERTIFICATE", block.Type)
			}
			certificate, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse PEM certificate: %w", err)
			}
			parsed = append(parsed, certificate)
			rest = bytes.TrimSpace(next)
		}
	} else {
		var err error
		parsed, err = x509.ParseCertificates(content)
		if err != nil {
			return nil, fmt.Errorf("parse DER certificate: %w", err)
		}
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("no certificates found")
	}
	return parsed, nil
}

func certificatesFromParsed(parsed []*x509.Certificate) []Certificate {
	byHash := make(map[string]Certificate, len(parsed))
	for _, certificate := range parsed {
		sum := sha256.Sum256(certificate.Raw)
		hash := hex.EncodeToString(sum[:])
		byHash[hash] = Certificate{
			Name:   "epar-" + hash[:24] + ".crt",
			PEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}),
			SHA256: hash,
		}
	}
	out := make([]Certificate, 0, len(byHash))
	for _, certificate := range byHash {
		out = append(out, certificate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SHA256 < out[j].SHA256 })
	return out
}

// Canonicalize validates and deduplicates a snapshot and computes its stable
// content generation. CollectedAt is intentionally excluded from Generation.
func Canonicalize(snapshot Snapshot) (Snapshot, error) {
	byHash := make(map[string]Certificate, len(snapshot.Certificates))
	for _, certificate := range snapshot.Certificates {
		parsed, err := CertificatesFromBytes(certificate.PEM)
		if err != nil || len(parsed) != 1 {
			if err == nil {
				err = fmt.Errorf("certificate PEM contains multiple certificates")
			}
			return Snapshot{}, fmt.Errorf("canonicalize host trust certificate %q: %w", certificate.Name, err)
		}
		canonical := parsed[0]
		if certificate.SHA256 != "" && certificate.SHA256 != canonical.SHA256 {
			return Snapshot{}, fmt.Errorf("certificate %q SHA-256 does not match PEM", certificate.Name)
		}
		byHash[canonical.SHA256] = canonical
	}
	snapshot.Certificates = snapshot.Certificates[:0]
	for _, certificate := range byHash {
		snapshot.Certificates = append(snapshot.Certificates, certificate)
	}
	sort.Slice(snapshot.Certificates, func(i, j int) bool { return snapshot.Certificates[i].SHA256 < snapshot.Certificates[j].SHA256 })

	snapshot.HostOS = strings.ToLower(strings.TrimSpace(snapshot.HostOS))
	snapshot.Scopes = sortedUnique(snapshot.Scopes)
	snapshot.Sources = sortedSources(snapshot.Sources)
	snapshot.Policy = sortedPolicy(snapshot.Policy)
	h := sha256.New()
	_, _ = h.Write([]byte(fmt.Sprintf("epar-hosttrust-schema=%d\n", canonicalSchemaVersion)))
	_, _ = h.Write([]byte("hostOS=" + snapshot.HostOS + "\n"))
	for _, scope := range snapshot.Scopes {
		_, _ = h.Write([]byte("scope=" + scope + "\n"))
	}
	for _, certificate := range snapshot.Certificates {
		_, _ = h.Write([]byte(certificate.SHA256))
		_, _ = h.Write([]byte{'\n'})
	}
	for _, entry := range snapshot.Policy {
		_, _ = h.Write([]byte(entry.Scope + "\x00" + entry.Kind + "\x00" + entry.SHA256 + "\n"))
	}
	snapshot.Generation = hex.EncodeToString(h.Sum(nil))
	return snapshot, nil
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func sortedSources(values []Source) []Source {
	out := append([]Source(nil), values...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func sortedPolicy(values []PolicyEntry) []PolicyEntry {
	seen := make(map[string]PolicyEntry, len(values))
	for _, value := range values {
		key := value.Scope + "\x00" + value.Kind + "\x00" + value.SHA256
		seen[key] = value
	}
	out := make([]PolicyEntry, 0, len(seen))
	for _, value := range seen {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].SHA256 < out[j].SHA256
	})
	return out
}

// MarkerJSON returns deterministic serializable image-manifest metadata. It
// omits CollectedAt so repeated collection of unchanged roots is cache-stable.
func MarkerJSON(snapshot Snapshot) ([]byte, error) {
	canonical, err := Canonicalize(snapshot)
	if err != nil {
		return nil, err
	}
	marker := struct {
		Generation   string   `json:"generation"`
		HostOS       string   `json:"hostOS"`
		Scopes       []string `json:"scopes"`
		Certificates []struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"certificates"`
		Policy []PolicyEntry `json:"policy,omitempty"`
	}{
		Generation: canonical.Generation,
		HostOS:     canonical.HostOS,
		Scopes:     canonical.Scopes,
		Policy:     canonical.Policy,
	}
	for _, certificate := range canonical.Certificates {
		marker.Certificates = append(marker.Certificates, struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		}{Name: certificate.Name, SHA256: certificate.SHA256})
	}
	return json.Marshal(marker)
}
