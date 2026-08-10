package invocation

import "testing"

func TestCommandPrefix(t *testing.T) {
	tests := []struct {
		name       string
		marker     string
		arg0       string
		executable string
		want       string
	}{
		{name: "start wrapper", marker: "start", arg0: "ignored", want: "./start"},
		{name: "docker shell wrapper", marker: "run-with-docker", arg0: "ignored", want: "scripts/run-with-docker.sh"},
		{name: "docker PowerShell wrapper", marker: "run-with-docker-powershell", arg0: "ignored", want: `scripts\run-with-docker.ps1`},
		{name: "go run Unix", arg0: "/tmp/go-build123/b001/exe/ephemeral-action-runner", executable: "/tmp/go-build123/b001/exe/ephemeral-action-runner", want: "go run ./cmd/ephemeral-action-runner"},
		{name: "go run Windows", arg0: `C:\Temp\go-build123\b001\exe\ephemeral-action-runner.exe`, executable: `C:\Temp\go-build123\b001\exe\ephemeral-action-runner.exe`, want: "go run ./cmd/ephemeral-action-runner"},
		{name: "direct binary", arg0: `.\bin\ephemeral-action-runner.exe`, executable: `C:\repo\bin\ephemeral-action-runner.exe`, want: `.\bin\ephemeral-action-runner.exe`},
		{name: "direct binary with spaces", arg0: `C:\EPAR Tools\ephemeral-action-runner.exe`, executable: `C:\EPAR Tools\ephemeral-action-runner.exe`, want: `"C:\EPAR Tools\ephemeral-action-runner.exe"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := commandPrefix(test.marker, test.arg0, test.executable); got != test.want {
				t.Fatalf("commandPrefix() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUnknownMarkerCannotReplaceCommand(t *testing.T) {
	got := commandPrefix("arbitrary command", "ephemeral-action-runner", `C:\bin\ephemeral-action-runner.exe`)
	if got != "ephemeral-action-runner" {
		t.Fatalf("commandPrefix() = %q, want direct binary", got)
	}
}

func TestScopedCommandPreservesConfigAndProjectRoot(t *testing.T) {
	t.Setenv(Environment, "start")
	got := ScopedCommand(`D:\repo\EPAR Config\config.yml`, `D:\repo\EPAR`, "storage", "status", "--provider", "docker-sandboxes")
	want := `./start storage status --provider docker-sandboxes --config "D:\repo\EPAR Config\config.yml" --project-root D:\repo\EPAR`
	if got != want {
		t.Fatalf("ScopedCommand() = %q, want %q", got, want)
	}
}
