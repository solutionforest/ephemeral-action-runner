# WSL Provider

This package implements the Windows WSL2 provider.

It imports disposable Ubuntu distros from a reusable rootfs tar, executes guest
commands through `wsl.exe -d <name> --user root --exec`, exports built images
with `wsl --export`, and unregisters completed distros with `wsl --unregister`.

`Start` keeps one quiet host-side `wsl.exe -d <name>` process open for each
running distro. WSL can auto-stop imported distros that only have systemd
services left, so this keepalive is required for registered runners to stay
online while waiting for jobs.

WSL2 is treated as trusted-job infrastructure. It is not equivalent to one full
hardware VM isolation boundary per job.
