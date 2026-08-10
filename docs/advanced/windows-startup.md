# Windows Startup

On Windows, EPAR can start after login with either a Startup folder shortcut or Task Scheduler.

Use the Startup folder for a personal machine where a visible foreground window is fine. Use Task Scheduler when you want delayed start, restart behavior, or a quieter background task.

Run EPAR manually once first so `.local\config.yml` exists. The first run can take a while because `start` may build or refresh the configured image before starting runners.

Startup automation must use the same `./start` command as a manual launch so first-run handling, local-Go and no-Go selection, argument forwarding, trust setup, and controller updates stay consistent. The examples below ask the Windows PowerShell executable included with Windows to resolve and invoke `./start`; users and automation never need to name EPAR's internal PowerShell file.

## Startup Folder Shortcut

Open the current user's Startup folder:

```powershell
Start-Process shell:startup
```

Create a shortcut that launches the wrapper through Windows PowerShell:

```text
Target: C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe
Arguments: -NoLogo -NoProfile -ExecutionPolicy Bypass -Command "& './start' --config .local\config.yml --external-outage-retry=continuous"
Start in: D:\path\to\ephemeral-action-runner
```

`Start in` is important. It keeps relative paths such as `.local\config.yml`, `work\logs`, `configs`, and `scripts` anchored to the EPAR folder.

You can also create the shortcut from PowerShell:

```powershell
$root = "D:\path\to\ephemeral-action-runner"
$powershell = Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe"
$startup = [Environment]::GetFolderPath("Startup")
$shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut("$startup\EPAR.lnk")
$shortcut.TargetPath = $powershell
$shortcut.WorkingDirectory = $root
$shortcut.Arguments = '-NoLogo -NoProfile -ExecutionPolicy Bypass -Command "& ''./start'' --config .local\config.yml --external-outage-retry=continuous"'
$shortcut.Save()
```

## Task Scheduler

Create a user logon task:

1. Open **Task Scheduler**.
2. Choose **Create Task**.
3. On **Triggers**, add **At log on**. Add a short delay if Docker needs time to start.
4. On **Actions**, choose **Start a program**.
5. Set **Program/script** to `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`.
6. Set **Add arguments** to `-NoLogo -NoProfile -ExecutionPolicy Bypass -Command "& './start' --config .local\config.yml --external-outage-retry=continuous"`.
7. Set **Start in** to `D:\path\to\ephemeral-action-runner`.

If the host runtime is tied to the user session, keep the task as a user logon task. A boot-time system task may start too early or without the expected Docker context.

PowerShell equivalent:

```powershell
$root = "D:\path\to\ephemeral-action-runner"
$powershell = Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe"
$arguments = '-NoLogo -NoProfile -ExecutionPolicy Bypass -Command "& ''./start'' --config .local\config.yml --external-outage-retry=continuous"'
$action = New-ScheduledTaskAction `
  -Execute $powershell `
  -Argument $arguments `
  -WorkingDirectory $root
$trigger = New-ScheduledTaskTrigger -AtLogOn
$trigger.Delay = "PT1M"
Register-ScheduledTask -TaskName "EPAR" -Action $action -Trigger $trigger -Description "Start EPAR at user logon" -Force
```

Start or stop it manually:

```powershell
Start-ScheduledTask -TaskName "EPAR"
Stop-ScheduledTask -TaskName "EPAR"
```

Delete it:

```powershell
Unregister-ScheduledTask -TaskName "EPAR" -Confirm:$false
```

## Notes

- The autorun examples use `--external-outage-retry=continuous` so a transient GitHub, registry, or runner-download outage pauses the existing controller instead of ending the task. Use a positive duration such as `--external-outage-retry=4h` when the task should eventually fail, but do not combine bounded mode with an unconditional task restart policy because each restart can otherwise obscure the exhausted result. EPAR persists the original bounded incident deadline for the selected config.
- Stop the foreground EPAR process or scheduled task to trigger normal cleanup.
- For Docker Container, the host Docker runtime must support privileged Linux containers.
- For WSL, make sure WSL2 is installed and the configured WSL image has been built or can be built by `start`.
- If the selected provider needs Docker, use a logon trigger with a delay so Docker has time to become ready.
