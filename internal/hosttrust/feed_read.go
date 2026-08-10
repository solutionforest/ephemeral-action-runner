package hosttrust

import (
	"errors"
	"io/fs"
	"os"
	"time"
)

const (
	feedReadAttempts   = 8
	feedReadRetryDelay = 10 * time.Millisecond
	feedReadRetryCap   = 80 * time.Millisecond
)

func readFeedFile(path string) ([]byte, error) {
	return readFeedFileWith(path, os.ReadFile, time.Sleep)
}

func readFeedFileWith(path string, read func(string) ([]byte, error), sleep func(time.Duration)) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < feedReadAttempts; attempt++ {
		content, err := read(path)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !isTransientFeedReadError(err) || attempt == feedReadAttempts-1 {
			break
		}
		delay := feedReadRetryDelay << attempt
		if delay > feedReadRetryCap {
			delay = feedReadRetryCap
		}
		sleep(delay)
	}
	return nil, lastErr
}

func isTransientFeedReadError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || isPlatformTransientFeedReadError(err)
}
