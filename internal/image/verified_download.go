package image

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

func verifiedDownload(ctx context.Context, client *http.Client, sourceURL, destination, expectedSHA256 string, mode os.FileMode) error {
	expectedSHA256 = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(expectedSHA256), "sha256:"))
	if len(expectedSHA256) != sha256.Size*2 {
		return fmt.Errorf("invalid locked SHA-256 for %s", sourceURL)
	}
	if ok, err := fileMatchesSHA256(destination, expectedSHA256); err != nil {
		return err
	} else if ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	partial := destination + ".partial"
	var offset int64
	if info, err := os.Lstat(partial); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("partial download %s is not a regular file", partial)
		}
		offset = info.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download %s: %w", sourceURL, err)
	}
	defer response.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	if response.StatusCode == http.StatusPartialContent && offset > 0 {
		flags |= os.O_APPEND
	} else {
		offset = 0
		flags |= os.O_TRUNC
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("download %s: HTTP %s", sourceURL, response.Status)
	}
	file, err := os.OpenFile(partial, flags, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, response.Body)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("download %s after %d bytes: %w", sourceURL, offset, copyErr)
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	ok, err := fileMatchesSHA256(partial, expectedSHA256)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("download %s failed locked SHA-256 verification; partial content retained at %s", sourceURL, partial)
	}
	if err := os.Chmod(partial, mode); err != nil {
		return err
	}
	if runtimeRenameReplace(partial, destination); err != nil {
		return err
	}
	return nil
}

func fileMatchesSHA256(path, expected string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("download target %s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == expected, nil
}

func buildTrustHTTPClient(snapshot hosttrust.Snapshot) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	for _, certificate := range snapshot.Certificates {
		if !roots.AppendCertsFromPEM(certificate.PEM) {
			return nil, fmt.Errorf("append operational build CA %s", certificate.Name)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &http.Client{Transport: transport}, nil
}

func runtimeRenameReplace(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(source, destination)
}
