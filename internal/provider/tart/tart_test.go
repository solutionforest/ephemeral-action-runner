package tart

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
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

func TestGuestAgentBootRetryPreservesDeadlineAndTransport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New("tart", false)
		attempts := 0
		p.runCommand = func(_ context.Context, _ io.Reader, _, _ io.Writer, args ...string) (provider.ExecResult, error) {
			attempts++
			if strings.Join(args, " ") != "ip runner --wait 5 --resolver agent" {
				t.Fatalf("unexpected boot transport: %v", args)
			}
			if attempts == 1 {
				return provider.ExecResult{}, errors.New("Error: RPC timed out before completing")
			}
			return provider.ExecResult{Stdout: "192.168.64.5\n"}, nil
		}
		address, err := p.IP(context.Background(), "runner", 5)
		if err != nil || address != "192.168.64.5" || attempts != 2 {
			t.Fatalf("address=%q attempts=%d err=%v", address, attempts, err)
		}
		p.runCommand = func(context.Context, io.Reader, io.Writer, io.Writer, ...string) (provider.ExecResult, error) {
			return provider.ExecResult{}, errors.New("RPC timed out")
		}
		if _, err := p.IP(context.Background(), "runner", 2); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("boot deadline lost: %v", err)
		}
	})
}

func TestGuestAgentBootDoesNotRetryPermanentFailure(t *testing.T) {
	p := New("tart", false)
	attempts := 0
	p.runCommand = func(context.Context, io.Reader, io.Writer, io.Writer, ...string) (provider.ExecResult, error) {
		attempts++
		return provider.ExecResult{}, errors.New("VM does not exist")
	}
	if _, err := p.IP(context.Background(), "missing", 180); err == nil || attempts != 1 {
		t.Fatalf("permanent failure attempts=%d err=%v", attempts, err)
	}
}

func TestGuestAgentBootRejectsResultAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := New("tart", false)
	p.runCommand = func(context.Context, io.Reader, io.Writer, io.Writer, ...string) (provider.ExecResult, error) {
		cancel()
		return provider.ExecResult{Stdout: "192.168.64.5"}, nil
	}
	if address, err := p.IP(ctx, "runner", 180); address != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("accepted canceled boot: address=%q err=%v", address, err)
	}
}

func TestCaptureWriterCopiesToCaptureAndInjectedSink(t *testing.T) {
	var capture, sink bytes.Buffer
	writer := captureWriter(&capture, &sink)
	if _, err := writer.Write([]byte("transcript")); err != nil {
		t.Fatal(err)
	}
	if capture.String() != "transcript" || sink.String() != "transcript" {
		t.Fatalf("capture=%q sink=%q", capture.String(), sink.String())
	}
}

func TestExecRedactsSensitiveValuesFromResultErrorAndSinks(t *testing.T) {
	const secret = "sentinel-registration-token"
	p := New("tart", false)
	var stdout, stderr bytes.Buffer
	p.runCommand = func(_ context.Context, _ io.Reader, gotStdout, gotStderr io.Writer, args ...string) (provider.ExecResult, error) {
		_, _ = gotStdout.Write([]byte("stream " + secret))
		_, _ = gotStderr.Write([]byte("RUNNER_TOKEN=" + secret))
		return provider.ExecResult{Stdout: "result " + secret, Stderr: "SECRET=" + secret}, errors.New("tart " + strings.Join(args, " ") + " failed with " + secret)
	}
	result, err := p.Exec(context.Background(), "runner", []string{"false"}, provider.ExecOptions{
		Env:             map[string]string{"RUNNER_TOKEN": secret},
		SensitiveValues: []string{secret},
		Stdout:          &stdout,
		Stderr:          &stderr,
	})
	combined := stdout.String() + stderr.String() + result.Stdout + result.Stderr + err.Error()
	if strings.Contains(combined, secret) {
		t.Fatalf("sensitive Tart exec leaked secret: %q", combined)
	}
}

func TestExecRedactsSecretAssignmentsWithoutSensitiveValues(t *testing.T) {
	const secret = "ordinary-sentinel"
	p := New("tart", false)
	var stdout, stderr bytes.Buffer
	p.runCommand = func(_ context.Context, _ io.Reader, gotStdout, gotStderr io.Writer, _ ...string) (provider.ExecResult, error) {
		_, _ = gotStdout.Write([]byte("PASSWORD=" + secret + "\n"))
		_, _ = gotStderr.Write([]byte("SECRET=" + secret + "\n"))
		return provider.ExecResult{Stdout: "TOKEN=" + secret, Stderr: "PRIVATE_KEY=" + secret}, errors.New("RUNNER_TOKEN=" + secret)
	}
	result, err := p.Exec(context.Background(), "runner", []string{"false"}, provider.ExecOptions{Stdout: &stdout, Stderr: &stderr})
	combined := stdout.String() + stderr.String() + result.Stdout + result.Stderr + err.Error()
	if strings.Contains(combined, secret) {
		t.Fatalf("ordinary Tart exec leaked secret assignment: %q", combined)
	}
}

func TestStartDryRunIncludesRosettaBeforeName(t *testing.T) {
	p := New("tart", true)
	out := captureStdout(t, func() {
		if _, err := p.Start(context.Background(), "epar-test", provider.StartOptions{RosettaTag: "rosetta"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "tart run --no-graphics --rosetta rosetta epar-test") {
		t.Fatalf("dry-run output did not include Rosetta start command: %q", out)
	}
}

func TestStartUsesOnlyExplicitNetworkMode(t *testing.T) {
	for _, test := range []struct {
		network string
		flag    string
		invalid bool
	}{
		{network: "default"},
		{network: "softnet", flag: "--net-softnet "},
		{network: "host", flag: "--net-host "},
		{network: "bridged:en0", flag: "--net-bridged=en0 "},
		{network: "bridged:", invalid: true},
		{network: "bridged:list", invalid: true},
		{network: "bridged", invalid: true},
	} {
		t.Run(test.network, func(t *testing.T) {
			p := New("tart", true)
			out := captureStdout(t, func() {
				_, err := p.Start(context.Background(), "epar-test", provider.StartOptions{Network: test.network})
				if (err != nil) != test.invalid {
					t.Fatalf("network=%q err=%v", test.network, err)
				}
			})
			if test.invalid {
				if out != "" {
					t.Fatalf("invalid network silently launched Tart: %q", out)
				}
			} else if want := "tart run --no-graphics " + test.flag + "epar-test"; !strings.Contains(out, want) {
				t.Fatalf("network=%q output=%q, want %q", test.network, out, want)
			}
		})
	}
}

func TestVMProcessSurvivesSuccessfulProvisioningContextCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "survived")
	helper := filepath.Join(directory, "tart-test-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 1\nprintf survived >\"$3.tmp\"\nmv \"$3.tmp\" \"$3\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(helper, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	process, err := p.Start(ctx, marker, provider.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		content, err := os.ReadFile(marker)
		if err == nil {
			if string(content) != "survived" || process.PID <= 0 {
				t.Fatalf("marker=%q process=%+v", content, process)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("Tart VM process was killed when successful provisioning finished")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestVMStartRejectsCanceledAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	process, err := New("unused-tart-binary", false).Start(ctx, "runner", provider.StartOptions{})
	if process != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("process=%+v err=%v, want canceled admission", process, err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	defer func() { os.Stdout = original }()

	fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestReadLocalVMIdentitiesUsesStableMACAddress(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TART_HOME", home)
	vmDirectory := filepath.Join(home, "vms", "epar-tart-runner")
	if err := os.MkdirAll(vmDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmDirectory, "config.json"), []byte(`{"macAddress":"02:00:00:12:34:56"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	identities, err := readLocalVMIdentities()
	if err != nil {
		t.Fatal(err)
	}
	if got := identities["epar-tart-runner"]; got != "02:00:00:12:34:56" {
		t.Fatalf("identity = %q", got)
	}
}

func TestCloneEnvironmentDisablesTartAutomaticPruning(t *testing.T) {
	environment := tartCloneEnvironment([]string{"PATH=/bin", "TART_NO_AUTO_PRUNE="})
	if got := environment[len(environment)-1]; got != "TART_NO_AUTO_PRUNE=1" {
		t.Fatalf("clone environment final override = %q", got)
	}
	count := 0
	for _, value := range environment {
		if strings.HasPrefix(value, "TART_NO_AUTO_PRUNE=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("clone environment contains %d TART_NO_AUTO_PRUNE entries: %#v", count, environment)
	}
}

func TestNativeMacOSRequiresValidatedTartRuntime(t *testing.T) {
	for _, test := range []struct {
		version string
		valid   bool
	}{
		{"2.32.1", false}, {"2.41.0", false}, {"2.41.1", true}, {"v2.42.0\n", true}, {"3.0.0", true}, {"unknown", false}, {"2.41.1-beta", false},
	} {
		t.Run(test.version, func(t *testing.T) {
			if err := ValidateMacOSVersion(test.version); (err == nil) != test.valid {
				t.Fatalf("version validation: %v", err)
			}
		})
	}
	p := New("tart", false)
	p.Platform = "darwin/arm64"
	p.runCommand = func(_ context.Context, _ io.Reader, _, _ io.Writer, args ...string) (provider.ExecResult, error) {
		if len(args) != 1 || args[0] != "--version" {
			t.Fatalf("old Tart attempted a clone: %v", args)
		}
		return provider.ExecResult{Stdout: "2.32.1"}, nil
	}
	if err := p.Clone(context.Background(), "source", "new-runner"); err == nil {
		t.Fatal("old native runtime was admitted")
	}
}
