package pool

import (
	"fmt"
	"strings"
	"time"
)

func RunnerNames(prefix string, count int, now time.Time) []string {
	names := make([]string, count)
	for i := 0; i < count; i++ {
		names[i] = RunnerName(prefix, i+1, now)
	}
	return names
}

func RunnerName(prefix string, sequence int, now time.Time) string {
	return fmt.Sprintf("%s-%s-%03d", prefix, now.Format("20060102-150405"), sequence)
}

func HasPrefix(name, prefix string) bool {
	return name == prefix || strings.HasPrefix(name, prefix+"-")
}
