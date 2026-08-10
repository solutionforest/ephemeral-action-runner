//go:build !windows

package hosttrust

func isPlatformTransientFeedReadError(error) bool {
	return false
}
