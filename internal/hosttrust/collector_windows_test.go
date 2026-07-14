//go:build windows

package hosttrust

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestWindowsCAAnchorsSkipsGoUnparseableStoreEntry(t *testing.T) {
	block, _ := pem.Decode(testCertificatePEM(t, true, "Windows Root"))
	if block == nil {
		t.Fatal("decode test certificate PEM")
	}
	valid := append([]byte(nil), block.Bytes...)
	negativeSerial := windowsTestNegativeSerialDER(t, valid)
	if _, err := x509.ParseCertificate(negativeSerial); err == nil || !strings.Contains(err.Error(), "negative serial number") {
		t.Fatalf("negative-serial fixture parse error = %v", err)
	}

	certificates, err := windowsCAAnchors([][]byte{negativeSerial, valid}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(certificates) != 1 {
		t.Fatalf("Windows CA anchor count = %d, want 1 valid anchor", len(certificates))
	}
	if _, err := CertificatesFromBytes(negativeSerial); err == nil {
		t.Fatal("strict explicit certificate parsing accepted the skipped native-store fixture")
	}
	now := time.Now().UTC()
	sum := sha256.Sum256(negativeSerial)
	feed, err := json.Marshal(FeedDocument{
		SchemaVersion: feedSchemaVersion,
		HostOS:        "windows",
		Scopes:        []string{ScopeSystem},
		GeneratedAt:   now,
		ExpiresAt:     now.Add(time.Minute),
		Certificates: []FeedCertificate{{
			SHA256: hex.EncodeToString(sum[:]),
			PEM:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: negativeSerial})),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFeed(feed, "", now, "negative-serial.json"); err == nil || !strings.Contains(err.Error(), "negative serial number") {
		t.Fatalf("strict feed parse error = %v", err)
	}
}

func windowsTestNegativeSerialDER(t *testing.T, valid []byte) []byte {
	t.Helper()
	der := append([]byte(nil), valid...)
	outerTag, outerContent, outerEnd := windowsTestDERElement(t, der, 0, len(der))
	if outerTag != 0x30 || outerEnd != len(der) {
		t.Fatal("test certificate outer sequence is malformed")
	}
	tbsTag, tbsContent, tbsEnd := windowsTestDERElement(t, der, outerContent, outerEnd)
	if tbsTag != 0x30 {
		t.Fatal("test certificate TBSCertificate is malformed")
	}
	serialOffset := tbsContent
	tag, _, end := windowsTestDERElement(t, der, serialOffset, tbsEnd)
	if tag == 0xa0 {
		serialOffset = end
	}
	serialTag, serialContent, serialEnd := windowsTestDERElement(t, der, serialOffset, tbsEnd)
	if serialTag != 0x02 || serialContent == serialEnd {
		t.Fatal("test certificate serial number is malformed")
	}
	der[serialContent] |= 0x80
	return der
}

func windowsTestDERElement(t *testing.T, der []byte, offset, limit int) (byte, int, int) {
	t.Helper()
	if offset < 0 || offset+2 > limit || limit > len(der) {
		t.Fatal("truncated test DER element")
	}
	tag := der[offset]
	offset++
	lengthByte := der[offset]
	offset++
	length := int(lengthByte)
	if lengthByte&0x80 != 0 {
		lengthBytes := int(lengthByte & 0x7f)
		if lengthBytes == 0 || lengthBytes > 4 || offset+lengthBytes > limit {
			t.Fatal("invalid test DER length")
		}
		length = 0
		for range lengthBytes {
			length = length<<8 | int(der[offset])
			offset++
		}
	}
	if length > limit-offset {
		t.Fatal("test DER length exceeds enclosing element")
	}
	return tag, offset, offset + length
}
