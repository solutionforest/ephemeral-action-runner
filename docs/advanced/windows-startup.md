# Windows Startup

On Windows, EPAR can start after login with either a Startup folder shortcut or Task Scheduler.

Use the Startup folder for a personal machine where a visible foreground window is fine. Use Task Scheduler when you want delayed start, restart behavior, or a quieter background task.

Run EPAR manually once first so `.local\config.yml` exists. The first run can take a while because `start` may build or refresh the configured image before starting runners.

## Startup Folder Shortcut

Open the current user's Startup folder:

```powershell
Start-Process shell:startup
```

Create a shortcut to the release binary:

```text
Target:   D:\path\to\ephemeral-action-runner\ephemeral-action-runner.exe
Start in: D:\path\to\ephemeral-action-runner
```

For a source checkout, point the target at the source-built binary instead:

```text
Target:   D:\path\to\ephemeral-action-runner\bin\ephemeral-action-runner.exe
Start in: D:\path\to\ephemeral-action-runner
```

`Start in` is important. It keeps relative paths such as `.local\config.yml`, `work\logs`, `configs`, and `scripts` anchored to the EPAR folder.

You can also create the shortcut from PowerShell:

```powershell
$root = "D:\path\to\ephemeral-action-runner"
$startup = [Environment]::GetFolderPath("Startup")
$shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut("$startup\EPAR.lnk")
$shortcut.TargetPath = Join-Path $root "ephemeral-action-runner.exe"
$shortcut.WorkingDirectory = $root
$shortcut.Arguments = "start --config .local\config.yml"
$shortcut.Save()
```

## Task Scheduler

Create a user logon task:

1. Open **Task Scheduler**.
2. Choose **Create Task**.
3. On **Triggers**, add **At log on**. Add a short delay if Docker Desktop or another Docker daemon needs time to start.
4. On **Actions**, choose **Start a program**.
5. Set **Program/script** to `D:\path\to\ephemeral-action-runner\ephemeral-action-runner.exe`.
6. Set **Add arguments** to `start --config .local\config.yml`.
7. Set **Start in** to `D:\path\to\ephemeral-action-runner`.

For Docker Desktop, keep the task as a user logon task. Docker Desktop is usually tied to the user session, so a boot-time system task may start too early or without the expected Docker context.

PowerShell equivalent:

```powershell
$root = "D:\path\to\ephemeral-action-runner"
$action = New-ScheduledTaskAction `
  -Execute (Join-Path $root "ephemeral-action-runner.exe") `
  -Argument "start --config .local\config.yml" `
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

- Stop the foreground EPAR process or scheduled task to trigger normal cleanup.
- For Docker-DinD, the host Docker runtime must support privileged Linux containers.
- For WSL, make sure WSL2 is installed and the configured WSL image has been built or can be built by `start`.
- If the selected provider needs Docker, use a logon trigger with a delay so Docker has time to become ready.
