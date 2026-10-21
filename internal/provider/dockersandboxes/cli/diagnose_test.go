package cli

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestDiagnoseFlagCompatibility(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		err    error
		legacy bool
	}{
		{name: "modern", output: `{"version":"1.0"}`},
		{name: "legacy", err: errors.New("sbx diagnose failed: exit status 1: Error: unknown flag: --format\nUsage: sbx diagnose"), legacy: true},
		{name: "operational failure", err: errors.New("daemon unavailable")},
		{name: "canceled", err: fmt.Errorf("unknown flag: --format: %w", context.Canceled)},
		{name: "deadline", err: errors.Join(context.DeadlineExceeded, errors.New("unknown flag: --format"))},
		{name: "other flag", err: errors.New("unknown flag: --formatting")},
		{name: "ambiguous output", output: `{"checks":[]}`, err: errors.New("unknown flag: --format")},
		{name: "invalid report", output: `{"version":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls [][]string
			output, err := Diagnose(context.Background(), func(_ context.Context, args []string) ([]byte, error) {
				calls = append(calls, args)
				if len(calls) == 1 {
					return []byte(test.output), test.err
				}
				return []byte("legacy report"), nil
			})
			wantCalls := [][]string{{"diagnose", "--format", "json"}}
			if test.legacy {
				wantCalls = append(wantCalls, []string{"diagnose", "--output", "json"})
				if err != nil || string(output) != "legacy report" {
					t.Fatalf("legacy result = %s, %v", output, err)
				}
			} else if !errors.Is(err, test.err) || string(output) != test.output {
				t.Fatalf("result = %s, %v; want %s, %v", output, err, test.output, test.err)
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, wantCalls)
			}
		})
	}
}
