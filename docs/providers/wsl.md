# WSL Provider

The WSL provider is planned but not implemented in this version. If
`provider.type: wsl` is used today, EPAR exits with a clear not-implemented
message.

The intended lifecycle is:

- create a disposable distro with `wsl --import`
- execute provisioning and runner commands with `wsl -d <name> --exec`
- stop completed runners with `wsl --terminate`
- delete disposable distros with `wsl --unregister`
- discover stale distros with `wsl --list --verbose`

Important caveats:

- WSL2 is not the same isolation boundary as a full VM per job.
- Docker support depends on the Windows/WSL Docker setup and systemd behavior.
- This provider should start with trusted internal jobs only.
