package provider

import "fmt"

func SupportedTypes() []string {
	return []string{"tart", "wsl"}
}

func UnsupportedTypeError(providerType string) error {
	return fmt.Errorf("unsupported provider.type %q", providerType)
}
