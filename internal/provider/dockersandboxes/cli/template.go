package cli

import "strings"

// TemplateCacheID validates an inventory image ID and returns EPAR's existing
// short cache identity. sbx v0.53+ preserves full sha256 IDs in structured output.
func TemplateCacheID(imageID string) (string, bool) {
	id := imageID
	length := 12
	if digest, full := strings.CutPrefix(imageID, "sha256:"); full {
		id = digest
		length = 64
	}
	if len(id) != length {
		return "", false
	}
	for _, character := range id {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return "", false
		}
	}
	return id[:12], true
}
