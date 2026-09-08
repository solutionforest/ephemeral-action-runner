package dockersandboxes

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestRecoveryAbsenceExitHelper(t *testing.T) {
	switch os.Getenv("EPAR_TEST_RECOVERY_ABSENCE_EXIT") {
	case "1":
		os.Exit(1)
	case "2":
		os.Exit(2)
	}
}

func TestRecoveryIdentityAbsenceRequiresIndependentExactDiagnostic(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exitFailure := func(code string) error {
		command := exec.Command(executable, "-test.run=^TestRecoveryAbsenceExitHelper$")
		command.Env = append(os.Environ(), "EPAR_TEST_RECOVERY_ABSENCE_EXIT="+code)
		err := command.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("helper did not return process exit: %v", err)
		}
		return err
	}
	exitOne, exitTwo := exitFailure("1"), exitFailure("2")
	absent := "ERROR: sandbox '" + testName + "' not found"
	for _, test := range []struct {
		name       string
		stdout     string
		stderr     string
		err        error
		wantAbsent bool
		wantErr    bool
	}{
		{name: "live inspect diagnostic", stderr: absent, err: exitOne, wantAbsent: true},
		{name: "known ls hint", stderr: absent + " (run 'sbx ls' to see your sandboxes)", err: exitOne, wantAbsent: true},
		{name: "wrong identity", stderr: "ERROR: sandbox 'other' not found", err: exitOne, wantErr: true},
		{name: "broad legacy 404 is insufficient", stderr: "status 404", err: exitOne, wantErr: true},
		{name: "success empty is not absence"},
		{name: "success malformed is not absence", stdout: "not json"},
		{name: "success existing is not absence", stdout: `{"name":"epar-sandbox-1"}`},
		{name: "conflicting stdout", stdout: `{"name":"epar-sandbox-1"}`, stderr: absent, err: exitOne, wantErr: true},
		{name: "mixed stderr is not proof", stderr: "transport failure\n" + absent, err: exitOne, wantErr: true},
		{name: "deadline with stale diagnostic", stderr: absent, err: errors.Join(context.DeadlineExceeded, exitOne), wantErr: true},
		{name: "cancellation with stale diagnostic", stderr: absent, err: errors.Join(context.Canceled, exitOne), wantErr: true},
		{name: "unexpected exit status", stderr: absent, err: exitTwo, wantErr: true},
		{name: "non-exit failure", stderr: absent, err: errors.New("cannot launch CLI"), wantErr: true},
		{name: "exit joined with containment failure", stderr: absent, err: errors.Join(exitOne, errors.New("process containment failed")), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := New("test-sbx")
			p.runCommand = func(ctx context.Context, request commandRequest) (provider.ExecResult, error) {
				if !reflect.DeepEqual(request.args, []string{"inspect", "--json", testName}) || request.timeout != providerReadbackTimeout {
					t.Fatalf("unexpected readback request: %#v", request)
				}
				return provider.ExecResult{Stdout: test.stdout, Stderr: test.stderr}, test.err
			}
			ctx := provider.WithControlPlaneRecoveryCoordinator(provider.WithControlPlaneLock(context.Background()))
			got, err := p.VerifyControlPlaneIdentityAbsent(ctx, testInstance)
			if got != test.wantAbsent || (err != nil) != test.wantErr {
				t.Fatalf("absence = %v, err = %v; want absence %v, error %v", got, err, test.wantAbsent, test.wantErr)
			}
		})
	}
}

func TestRecoveryIdentityAbsenceRequiresLeaseAndValidIdentity(t *testing.T) {
	p := New("test-sbx")
	p.runCommand = func(context.Context, commandRequest) (provider.ExecResult, error) {
		t.Fatal("uncoordinated or invalid absence request executed a command")
		return provider.ExecResult{}, nil
	}
	contexts := []context.Context{context.Background(), provider.WithControlPlaneLock(context.Background()), provider.WithControlPlaneRecoveryCoordinator(context.Background())}
	for _, ctx := range contexts {
		if absent, err := p.VerifyControlPlaneIdentityAbsent(ctx, testInstance); absent || err == nil {
			t.Fatalf("uncoordinated absence = %v, error = %v", absent, err)
		}
	}
	ctx := provider.WithControlPlaneRecoveryCoordinator(provider.WithControlPlaneLock(context.Background()))
	if absent, err := p.VerifyControlPlaneIdentityAbsent(ctx, provider.Instance{Name: testName}); absent || err == nil {
		t.Fatalf("identityless absence = %v, error = %v", absent, err)
	}
}
