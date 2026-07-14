package hosttrust

import (
	"context"
	"runtime"
	"testing"
)

// This smoke is deliberately read-only: every native collector enumerates the
// effective host trust inputs but never writes a host trust store.
func TestNativeCollectorReadOnlySmoke(t *testing.T) {
	scopes := []string{ScopeSystem}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		scopes = append(scopes, ScopeUser)
	}
	snapshot, err := Resolve(context.Background(), Options{
		Mode:             ModeOverlay,
		Scopes:           scopes,
		ControllerHostOS: runtime.GOOS,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation == "" || len(snapshot.Certificates) == 0 {
		t.Fatalf("native collector returned generation %q with %d certificates", snapshot.Generation, len(snapshot.Certificates))
	}
}
