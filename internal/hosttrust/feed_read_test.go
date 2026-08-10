package hosttrust

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"time"
)

func TestReadFeedFileRetriesTransientReplacementGap(t *testing.T) {
	attempts := 0
	var delays []time.Duration
	content, err := readFeedFileWith("current.json", func(string) ([]byte, error) {
		attempts++
		if attempts < 4 {
			return nil, fs.ErrNotExist
		}
		return []byte("complete"), nil
	}, func(delay time.Duration) {
		delays = append(delays, delay)
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "complete" || attempts != 4 {
		t.Fatalf("content = %q, attempts = %d", content, attempts)
	}
	if want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}; !reflect.DeepEqual(delays, want) {
		t.Fatalf("retry delays = %v, want %v", delays, want)
	}
}

func TestReadFeedFileBoundsPersistentTransientFailure(t *testing.T) {
	attempts := 0
	sleeps := 0
	_, err := readFeedFileWith("current.json", func(string) ([]byte, error) {
		attempts++
		return nil, fs.ErrNotExist
	}, func(time.Duration) {
		sleeps++
	})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want not-exist", err)
	}
	if attempts != feedReadAttempts || sleeps != feedReadAttempts-1 {
		t.Fatalf("attempts = %d, sleeps = %d, want %d and %d", attempts, sleeps, feedReadAttempts, feedReadAttempts-1)
	}
}

func TestReadFeedFileDoesNotRetrySemanticOrPermissionFailure(t *testing.T) {
	attempts := 0
	wantErr := fs.ErrPermission
	_, err := readFeedFileWith("current.json", func(string) ([]byte, error) {
		attempts++
		return nil, wantErr
	}, func(time.Duration) {
		t.Fatal("non-transient failure slept before returning")
	})
	if !errors.Is(err, wantErr) || attempts != 1 {
		t.Fatalf("error = %v, attempts = %d", err, attempts)
	}
}
