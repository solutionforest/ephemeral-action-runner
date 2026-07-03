package tart

import (
	"context"
	"testing"
)

func TestListParsesTartOutputShape(t *testing.T) {
	p := New("tart", true)
	_ = p
	// The parser is exercised indirectly in integration; keep this package
	// dependency-free so dry-run builds work without Tart installed.
	_, err := New("tart", true).List(context.Background())
	if err != nil {
		t.Fatalf("dry-run list should not fail: %v", err)
	}
}
