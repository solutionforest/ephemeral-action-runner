//go:build linux

package main

import (
	"slices"
	"testing"
)

func TestHostTrustHookInvocationRequiresExactArgument(t *testing.T) {
	if !isHostTrustHookInvocation([]string{"--noprofile", "--norc", hookPath}) {
		t.Fatal("exact hook path was not recognized")
	}
	for _, arguments := range [][]string{
		{hookPath + ".bak"},
		{"echo " + hookPath},
		{"/tmp/check-host-trust-generation.sh"},
	} {
		if isHostTrustHookInvocation(arguments) {
			t.Fatalf("non-exact hook invocation was recognized: %q", arguments)
		}
	}
}

func TestIsolatedHookEnvironmentPassesOnlySafeLocaleAndGithubEnvInputs(t *testing.T) {
	got := isolatedHookEnvironment([]string{
		"HOME=/home/agent",
		"LANG=C.UTF-8",
		"LC_ALL=C",
		"TZ=UTC",
		"PATH=/tmp/attacker:/usr/bin",
		"BASH_ENV=/tmp/attack.sh",
		"ENV=/tmp/attack.sh",
		"PYTHONPATH=/tmp/attacker",
		"PYTHONSTARTUP=/tmp/attack.py",
		"LD_PRELOAD=/tmp/attack.so",
		"DOTNET_STARTUP_HOOKS=/tmp/attack.dll",
		"ACTIONS_RUNTIME_TOKEN=sentinel",
		"GITHUB_TOKEN=sentinel",
		"GITHUB_ENV=/opt/actions-runner/_work/_temp/_runner_file_commands/set_env_123",
	})
	want := []string{
		"LANG=C.UTF-8",
		"LC_ALL=C",
		"TZ=UTC",
		"GITHUB_ENV=/opt/actions-runner/_work/_temp/_runner_file_commands/set_env_123",
		"PATH=/usr/bin:/bin",
		"EPAR_HOOK_LAUNCHER=isolated-v1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("isolated environment mismatch\n got: %q\nwant: %q", got, want)
	}
}
