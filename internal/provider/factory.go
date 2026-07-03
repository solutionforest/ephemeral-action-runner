package provider

import "fmt"

func SupportedTypes() []string {
	return []string{"tart", "wsl"}
}

func WSLNotImplementedError() error {
	return fmt.Errorf("provider.type=wsl is documented but not implemented yet; see docs/providers/wsl.md")
}

func UnsupportedTypeError(providerType string) error {
	return fmt.Errorf("unsupported provider.type %q", providerType)
}
