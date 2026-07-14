//go:build windows

package hosttrust

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"syscall"
	"unsafe"
)

const (
	certSystemStoreCurrentUser  = 0x00010000
	certSystemStoreLocalMachine = 0x00020000
	certStoreProvSystemW        = 10
	certStoreOpenExistingFlag   = 0x00004000
	certStoreReadOnlyFlag       = 0x00008000
	errorNoMoreItems            = syscall.Errno(259)
	cryptENotFound              = syscall.Errno(0x80092004)
)

func collectNative(_ context.Context, scopes []string) (Snapshot, error) {
	var snapshot Snapshot
	denied, err := collectWindowsDisallowed(scopes)
	if err != nil {
		return Snapshot{}, err
	}
	for hash := range denied {
		snapshot.Policy = append(snapshot.Policy, PolicyEntry{Scope: "windows", Kind: "disallowed", SHA256: hash})
	}
	for _, scope := range scopes {
		switch scope {
		case ScopeSystem:
			der, err := collectWindowsStore("Root", certSystemStoreLocalMachine)
			if err != nil {
				return Snapshot{}, fmt.Errorf("collect Windows LocalMachine Root: %w", err)
			}
			certificates, err := windowsCAAnchors(der, denied)
			if err != nil {
				return Snapshot{}, err
			}
			snapshot.Certificates = append(snapshot.Certificates, certificates...)
			snapshot.Sources = append(snapshot.Sources, Source{Scope: ScopeSystem, Kind: "windows-local-machine-root"})
		case ScopeUser:
			der, err := collectWindowsStore("Root", certSystemStoreCurrentUser)
			if err != nil {
				return Snapshot{}, fmt.Errorf("collect Windows CurrentUser Root: %w", err)
			}
			certificates, err := windowsCAAnchors(der, denied)
			if err != nil {
				return Snapshot{}, err
			}
			snapshot.Certificates = append(snapshot.Certificates, certificates...)
			snapshot.Sources = append(snapshot.Sources, Source{Scope: ScopeUser, Kind: "windows-current-user-root"})
		}
	}
	return snapshot, nil
}

func windowsCAAnchors(derCertificates [][]byte, denied map[string]struct{}) ([]Certificate, error) {
	var out []Certificate
	for _, der := range derCertificates {
		sum := sha256.Sum256(der)
		if _, blocked := denied[hex.EncodeToString(sum[:])]; blocked {
			continue
		}
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			// Windows can carry legacy roots which CryptoAPI accepts but Go's
			// stricter X.509 parser rejects (for example, a negative serial
			// number). Skip only the unreadable native-store entry. Explicit
			// certificates and external feeds continue through their strict
			// validation paths.
			continue
		}
		if !certificate.IsCA {
			continue
		}
		certificates, err := CertificatesFromBytes(der)
		if err != nil {
			return nil, err
		}
		out = append(out, certificates...)
	}
	return out, nil
}

func collectWindowsDisallowed(scopes []string) (map[string]struct{}, error) {
	blocked := make(map[string]struct{})
	var flags []uint32
	if hasScope(scopes, ScopeSystem) {
		flags = append(flags, certSystemStoreLocalMachine)
	}
	if hasScope(scopes, ScopeUser) {
		flags = append(flags, certSystemStoreCurrentUser)
	}
	for _, flag := range flags {
		der, err := collectWindowsStore("Disallowed", flag)
		if err != nil {
			return nil, fmt.Errorf("collect Windows Disallowed store: %w", err)
		}
		for _, certificate := range der {
			sum := sha256.Sum256(certificate)
			blocked[hex.EncodeToString(sum[:])] = struct{}{}
		}
	}
	return blocked, nil
}

func collectWindowsStore(name string, flags uint32) ([][]byte, error) {
	nameUTF16, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	store, err := syscall.CertOpenStore(
		uintptr(certStoreProvSystemW),
		0,
		0,
		flags|certStoreOpenExistingFlag|certStoreReadOnlyFlag,
		uintptr(unsafe.Pointer(nameUTF16)),
	)
	if err != nil {
		return nil, err
	}
	defer syscall.CertCloseStore(store, 0)

	var previous *syscall.CertContext
	var certificates [][]byte
	for {
		current, enumErr := syscall.CertEnumCertificatesInStore(store, previous)
		previous = current
		if current == nil {
			if enumErr == syscall.Errno(0) || enumErr == errorNoMoreItems || enumErr == cryptENotFound {
				return certificates, nil
			}
			return nil, enumErr
		}
		if current.EncodedCert == nil || current.Length == 0 {
			continue
		}
		encoded := unsafe.Slice(current.EncodedCert, int(current.Length))
		certificates = append(certificates, append([]byte(nil), encoded...))
	}
}
