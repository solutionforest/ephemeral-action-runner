package pool

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMacOSRunnerHealthRejectsStaleProcessIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix guest script execution")
	}
	dir := t.TempDir()
	commands := map[string]string{
		"cat":       `case "$1" in *.start) echo expected-start ;; *) echo 12345 ;; esac`,
		"ps":        `case "$*" in *lstart=*) echo "${TEST_START:-expected-start}" ;; *uid=*) echo "${TEST_UID:-502}" ;; *stat=*) echo "${TEST_STATE:-S}" ;; *) exit 1 ;; esac`,
		"id":        `echo 502`,
		"launchctl": `test "${TEST_SERVICE:-running}" = running || exit 1; echo 'pid = 12345'`,
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\nset -eu\n"+body+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name, override string
		wantOK         bool
	}{
		{"same owned process", "", true},
		{"reused PID", "TEST_START=different-start", false},
		{"different owner", "TEST_UID=503", false},
		{"zombie", "TEST_STATE=Z", false},
		{"stopped service", "TEST_SERVICE=stopped", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command("bash", filepath.Join("..", "..", "scripts", "guest", "macos", "check-runner.sh"))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
			if test.override != "" {
				cmd.Env = append(cmd.Env, test.override)
			}
			output, err := cmd.CombinedOutput()
			if (err == nil) != test.wantOK {
				t.Fatalf("health result=%v output=%s", err, output)
			}
		})
	}
}
