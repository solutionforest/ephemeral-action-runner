package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	feedSchemaVersion = 1
	maxFeedBytes      = 32 << 20
	maxFeedAge        = 30 * time.Second
	maxCertificates   = 4096
)

type feedDocument struct {
	SchemaVersion  int               `json:"schemaVersion"`
	HostOS         string            `json:"hostOS"`
	Scopes         []string          `json:"scopes"`
	GeneratedAt    time.Time         `json:"generatedAt"`
	ExpiresAt      time.Time         `json:"expiresAt"`
	Certificates   []feedCertificate `json:"certificates"`
	DistrustSHA256 []string          `json:"distrustSHA256"`
}

type feedCertificate struct {
	SHA256 string `json:"sha256"`
	PEM    string `json:"pem"`
}

type bundleSummary struct {
	HostOS       string
	Scopes       []string
	Certificates int
	FeedSHA256   string
	BundleSHA256 string
}

func main() {
	feedPath := flag.String("feed", "", "host trust feed path")
	outputPath := flag.String("output", "", "validated PEM bundle output path")
	expectedHostOS := flag.String("expected-host-os", "", "expected feed host OS")
	flag.Parse()
	if flag.NArg() != 0 || strings.TrimSpace(*feedPath) == "" || strings.TrimSpace(*outputPath) == "" || strings.TrimSpace(*expectedHostOS) == "" {
		fmt.Fprintln(os.Stderr, "usage: bootstrap-trust --feed <path> --output <path> --expected-host-os <windows|darwin|linux>")
		os.Exit(2)
	}
	summary, err := materializeBundle(*feedPath, *outputPath, *expectedHostOS, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap build trust rejected: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("hostOS=%s scopes=%s certificates=%d feedSHA256=%s bundleSHA256=%s\n", summary.HostOS, strings.Join(summary.Scopes, ","), summary.Certificates, summary.FeedSHA256, summary.BundleSHA256)
}

func materializeBundle(feedPath, outputPath, expectedHostOS string, now time.Time) (bundleSummary, error) {
	feedPath = filepath.Clean(feedPath)
	info, err := os.Lstat(feedPath)
	if err != nil {
		return bundleSummary{}, fmt.Errorf("inspect feed: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return bundleSummary{}, fmt.Errorf("feed must be a regular non-symlink file")
	}
	file, err := os.Open(feedPath)
	if err != nil {
		return bundleSummary{}, fmt.Errorf("open feed: %w", err)
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maxFeedBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return bundleSummary{}, fmt.Errorf("read feed: %w", readErr)
	}
	if closeErr != nil {
		return bundleSummary{}, fmt.Errorf("close feed: %w", closeErr)
	}
	if len(content) > maxFeedBytes {
		return bundleSummary{}, fmt.Errorf("feed exceeds %d bytes", maxFeedBytes)
	}
	feedHash := sha256.Sum256(content)

	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var document feedDocument
	if err := decoder.Decode(&document); err != nil {
		return bundleSummary{}, fmt.Errorf("parse feed: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return bundleSummary{}, err
	}
	hostOS := strings.ToLower(strings.TrimSpace(document.HostOS))
	expected := strings.ToLower(strings.TrimSpace(expectedHostOS))
	if document.SchemaVersion != feedSchemaVersion {
		return bundleSummary{}, fmt.Errorf("unsupported schemaVersion %d", document.SchemaVersion)
	}
	if expected != "windows" && expected != "darwin" && expected != "linux" {
		return bundleSummary{}, fmt.Errorf("unsupported expected host OS %q", expectedHostOS)
	}
	if hostOS != expected {
		return bundleSummary{}, fmt.Errorf("feed hostOS %q does not match expected %q", hostOS, expected)
	}
	scopes, err := validateScopes(document.Scopes, hostOS)
	if err != nil {
		return bundleSummary{}, err
	}
	now = now.UTC()
	generatedAt := document.GeneratedAt.UTC()
	expiresAt := document.ExpiresAt.UTC()
	if generatedAt.IsZero() || expiresAt.IsZero() || !expiresAt.After(generatedAt) {
		return bundleSummary{}, fmt.Errorf("feed has invalid generatedAt/expiresAt")
	}
	if generatedAt.After(now.Add(5 * time.Second)) {
		return bundleSummary{}, fmt.Errorf("feed generatedAt is in the future")
	}
	if now.Sub(generatedAt) > maxFeedAge {
		return bundleSummary{}, fmt.Errorf("feed is older than %s", maxFeedAge)
	}
	if now.After(expiresAt) {
		return bundleSummary{}, fmt.Errorf("feed expired at %s", expiresAt.Format(time.RFC3339))
	}
	if len(document.Certificates) == 0 || len(document.Certificates) > maxCertificates {
		return bundleSummary{}, fmt.Errorf("feed certificate count %d is outside 1..%d", len(document.Certificates), maxCertificates)
	}

	distrusted := make(map[string]struct{}, len(document.DistrustSHA256))
	for index, value := range document.DistrustSHA256 {
		hash, err := normalizedSHA256(value)
		if err != nil {
			return bundleSummary{}, fmt.Errorf("distrustSHA256[%d]: %w", index, err)
		}
		if _, exists := distrusted[hash]; exists {
			return bundleSummary{}, fmt.Errorf("distrustSHA256[%d] duplicates %s", index, hash)
		}
		distrusted[hash] = struct{}{}
	}

	certificates := make(map[string][]byte, len(document.Certificates))
	for index, encoded := range document.Certificates {
		declaredHash, err := normalizedSHA256(encoded.SHA256)
		if err != nil {
			return bundleSummary{}, fmt.Errorf("certificate %d SHA-256: %w", index, err)
		}
		if _, exists := certificates[declaredHash]; exists {
			return bundleSummary{}, fmt.Errorf("certificate %d duplicates %s", index, declaredHash)
		}
		block, rest := pem.Decode([]byte(encoded.PEM))
		if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
			return bundleSummary{}, fmt.Errorf("certificate %d must contain exactly one CERTIFICATE PEM block", index)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return bundleSummary{}, fmt.Errorf("certificate %d parse: %w", index, err)
		}
		if !certificate.IsCA || !certificate.BasicConstraintsValid {
			return bundleSummary{}, fmt.Errorf("certificate %d is not a valid CA certificate", index)
		}
		actualHash := sha256.Sum256(block.Bytes)
		actualHex := hex.EncodeToString(actualHash[:])
		if actualHex != declaredHash {
			return bundleSummary{}, fmt.Errorf("certificate %d SHA-256 mismatch: declared %s, got %s", index, declaredHash, actualHex)
		}
		if _, denied := distrusted[actualHex]; denied {
			return bundleSummary{}, fmt.Errorf("certificate %d is also present in distrustSHA256", index)
		}
		certificates[actualHex] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})
	}

	hashes := make([]string, 0, len(certificates))
	for hash := range certificates {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	var bundle bytes.Buffer
	for _, hash := range hashes {
		bundle.Write(certificates[hash])
	}
	bundleHash := sha256.Sum256(bundle.Bytes())
	if err := writeAtomically(outputPath, bundle.Bytes()); err != nil {
		return bundleSummary{}, err
	}
	return bundleSummary{
		HostOS:       hostOS,
		Scopes:       scopes,
		Certificates: len(hashes),
		FeedSHA256:   hex.EncodeToString(feedHash[:]),
		BundleSHA256: hex.EncodeToString(bundleHash[:]),
	}, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("feed contains multiple JSON values")
	}
	return fmt.Errorf("parse trailing feed content: %w", err)
}

func validateScopes(values []string, hostOS string) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("feed scopes are empty")
	}
	seen := make(map[string]struct{}, len(values))
	scopes := make([]string, 0, len(values))
	for index, value := range values {
		scope := strings.ToLower(strings.TrimSpace(value))
		if scope != "system" && scope != "user" {
			return nil, fmt.Errorf("feed scope %d is unsupported: %q", index, value)
		}
		if hostOS == "linux" && scope == "user" {
			return nil, fmt.Errorf("Linux build trust cannot declare user scope")
		}
		if _, exists := seen[scope]; exists {
			return nil, fmt.Errorf("feed scope %d duplicates %q", index, scope)
		}
		seen[scope] = struct{}{}
		scopes = append(scopes, scope)
	}
	if _, exists := seen["system"]; !exists {
		return nil, fmt.Errorf("build trust feed must include system scope")
	}
	sort.Strings(scopes)
	return scopes, nil
}

func normalizedSHA256(value string) (string, error) {
	hash := strings.ToLower(strings.TrimSpace(value))
	if len(hash) != sha256.Size*2 {
		return "", fmt.Errorf("must be %d hexadecimal characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("invalid hexadecimal value: %w", err)
	}
	return hash, nil
}

func writeAtomically(path string, content []byte) (err error) {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create bundle directory: %w", err)
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundle output must be a regular non-symlink file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect bundle output: %w", statErr)
	}
	temporary, err := os.CreateTemp(parent, ".bootstrap-ca-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary bundle: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporary != nil {
			_ = temporary.Close()
		}
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect temporary bundle: %w", err)
	}
	if _, err = temporary.Write(content); err != nil {
		return fmt.Errorf("write temporary bundle: %w", err)
	}
	if err = temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary bundle: %w", err)
	}
	if err = temporary.Close(); err != nil {
		temporary = nil
		return fmt.Errorf("close temporary bundle: %w", err)
	}
	temporary = nil
	if err = os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("activate bundle: %w", err)
	}
	return nil
}
