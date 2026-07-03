# WSL Provider

The WSL provider is intentionally scaffolded but not implemented yet.

The planned mapping is:

- clone/import: `wsl --import <name> <install-dir> <rootfs.tar>`
- start/exec: `wsl -d <name> --exec <command>`
- stop: `wsl --terminate <name>`
- delete: `wsl --unregister <name>`
- list: `wsl --list --verbose`

WSL2 instances share the WSL kernel and are not equivalent to separate hardware
virtual machines. Use this provider only for trusted jobs unless the isolation
model is reviewed for your environment.
