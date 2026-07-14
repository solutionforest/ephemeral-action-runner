package hosttrust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	feedSchemaVersion = 1
	maxFeedAge        = 30 * time.Second
)

// FeedDocument is the self-contained, atomically replaceable host-collector
// handoff format. Each PEM is independently pinned by its SHA-256.
type FeedDocument struct {
	SchemaVersion  int               `json:"schemaVersion"`
	HostOS         string            `json:"hostOS"`
	Scopes         []string          `json:"scopes"`
	GeneratedAt    time.Time         `json:"generatedAt"`
	ExpiresAt      time.Time         `json:"expiresAt"`
	Certificates   []FeedCertificate `json:"certificates"`
	DistrustSHA256 []string          `json:"distrustSHA256,omitempty"`
}

type FeedCertificate struct {
	SHA256 string `json:"sha256"`
	PEM    string `json:"pem"`
}

// ReadFeed reads and verifies an external feed before converting its CA
// certificates into a snapshot fragment.
func ReadFeed(path, expectedSHA256 string, now time.Time) (Snapshot, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read host trust feed %s: %w", path, err)
	}
	return ParseFeed(content, expectedSHA256, now, path)
}

// ParseFeed verifies feed freshness, an optional out-of-band document hash,
// and every embedded certificate hash before accepting it.
func ParseFeed(content []byte, expectedSHA256 string, now time.Time, source string) (Snapshot, error) {
	actual := sha256.Sum256(content)
	actualHex := hex.EncodeToString(actual[:])
	if expectedSHA256 != "" {
		expected := strings.ToLower(strings.TrimSpace(expectedSHA256))
		if len(expected) != sha256.Size*2 {
			return Snapshot{}, fmt.Errorf("host trust feed SHA-256 must be %d hexadecimal characters", sha256.Size*2)
		}
		if _, err := hex.DecodeString(expected); err != nil {
			return Snapshot{}, fmt.Errorf("host trust feed SHA-256 is invalid: %w", err)
		}
		if expected != actualHex {
			return Snapshot{}, fmt.Errorf("host trust feed SHA-256 mismatch: got %s", actualHex)
		}
	}
	var feed FeedDocument
	if err := json.Unmarshal(content, &feed); err != nil {
		return Snapshot{}, fmt.Errorf("parse host trust feed: %w", err)
	}
	if feed.SchemaVersion != feedSchemaVersion {
		return Snapshot{}, fmt.Errorf("unsupported host trust feed schemaVersion %d", feed.SchemaVersion)
	}
	hostOS := strings.ToLower(strings.TrimSpace(feed.HostOS))
	if hostOS != "windows" && hostOS != "darwin" && hostOS != "linux" {
		return Snapshot{}, fmt.Errorf("unsupported host trust feed hostOS %q", feed.HostOS)
	}
	scopes, err := normalizeScopes(feed.Scopes)
	if err != nil {
		return Snapshot{}, fmt.Errorf("host trust feed scopes: %w", err)
	}
	if hostOS == "linux" && hasScope(scopes, ScopeUser) {
		return Snapshot{}, fmt.Errorf("host trust feed for Linux cannot declare user scope")
	}
	if feed.GeneratedAt.IsZero() || feed.ExpiresAt.IsZero() || !feed.ExpiresAt.After(feed.GeneratedAt) {
		return Snapshot{}, fmt.Errorf("host trust feed has invalid generatedAt/expiresAt")
	}
	now = now.UTC()
	generatedAt := feed.GeneratedAt.UTC()
	if generatedAt.After(now.Add(5 * time.Second)) {
		return Snapshot{}, fmt.Errorf("host trust feed generatedAt is in the future")
	}
	if now.Sub(generatedAt) > maxFeedAge {
		return Snapshot{}, fmt.Errorf("host trust feed is older than %s", maxFeedAge)
	}
	if now.After(feed.ExpiresAt.UTC()) {
		return Snapshot{}, fmt.Errorf("host trust feed expired at %s", feed.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if len(feed.Certificates) == 0 {
		return Snapshot{}, fmt.Errorf("host trust feed contains no certificates")
	}
	certificates := make([]Certificate, 0, len(feed.Certificates))
	for index, encoded := range feed.Certificates {
		parsed, err := CertificatesFromBytes([]byte(encoded.PEM))
		if err != nil {
			return Snapshot{}, fmt.Errorf("host trust feed certificate %d: %w", index, err)
		}
		if len(parsed) != 1 {
			return Snapshot{}, fmt.Errorf("host trust feed certificate %d must contain exactly one certificate", index)
		}
		declaredHash := strings.ToLower(strings.TrimSpace(encoded.SHA256))
		if declaredHash == "" || declaredHash != parsed[0].SHA256 {
			return Snapshot{}, fmt.Errorf("host trust feed certificate %d SHA-256 does not match PEM", index)
		}
		certificates = append(certificates, parsed[0])
	}
	policy := make([]PolicyEntry, 0, len(feed.DistrustSHA256))
	for index, value := range feed.DistrustSHA256 {
		hash := strings.ToLower(strings.TrimSpace(value))
		if len(hash) != sha256.Size*2 {
			return Snapshot{}, fmt.Errorf("host trust feed distrustSHA256[%d] must be a full SHA-256", index)
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return Snapshot{}, fmt.Errorf("host trust feed distrustSHA256[%d] is invalid: %w", index, err)
		}
		policy = append(policy, PolicyEntry{Scope: hostOS, Kind: "disallowed", SHA256: hash})
	}
	return Canonicalize(Snapshot{
		HostOS:       hostOS,
		Certificates: certificates,
		Policy:       policy,
		Scopes:       scopes,
		Sources:      []Source{{Scope: "external", Kind: "feed", Path: source + "#sha256=" + actualHex}},
		CollectedAt:  now,
	})
}
