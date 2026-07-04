#!/usr/bin/env bash
set -euo pipefail

rosetta_tag="${EPAR_ROSETTA_TAG:-rosetta}"
rosetta_mount="${EPAR_ROSETTA_MOUNT:-/run/rosetta}"

if [[ ! "${rosetta_tag}" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$ ]]; then
  echo "invalid EPAR_ROSETTA_TAG: ${rosetta_tag}" >&2
  exit 1
fi

install -d -m 0755 /opt/epar /opt/epar/features

cat >/opt/epar/setup-rosetta.sh <<'SETUP_ROSETTA'
#!/usr/bin/env bash
set -euo pipefail

rosetta_tag="${EPAR_ROSETTA_TAG:-rosetta}"
rosetta_mount="${EPAR_ROSETTA_MOUNT:-/run/rosetta}"

if [[ ! "${rosetta_tag}" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$ ]]; then
  echo "invalid EPAR_ROSETTA_TAG: ${rosetta_tag}" >&2
  exit 1
fi

install -d -m 0755 "${rosetta_mount}"
if ! mountpoint -q "${rosetta_mount}"; then
  mount -t virtiofs "${rosetta_tag}" "${rosetta_mount}"
fi

if [[ ! -x "${rosetta_mount}/rosetta" ]]; then
  echo "Rosetta executable not found at ${rosetta_mount}/rosetta" >&2
  exit 1
fi

modprobe binfmt_misc 2>/dev/null || true
if ! mountpoint -q /proc/sys/fs/binfmt_misc; then
  mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc
fi

if [[ ! -w /proc/sys/fs/binfmt_misc/register ]]; then
  echo "binfmt_misc register file is not writable" >&2
  exit 1
fi

if [[ -f /proc/sys/fs/binfmt_misc/rosetta ]]; then
  echo -1 >/proc/sys/fs/binfmt_misc/rosetta || true
fi

printf '%s' ':rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xff\xff\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:'"${rosetta_mount}"'/rosetta:OCF' >/proc/sys/fs/binfmt_misc/register

if [[ ! -f /proc/sys/fs/binfmt_misc/rosetta ]]; then
  echo "Rosetta binfmt registration did not appear" >&2
  exit 1
fi
SETUP_ROSETTA

chmod 0755 /opt/epar/setup-rosetta.sh

cat >/etc/systemd/system/epar-rosetta.service <<EOF_SERVICE
[Unit]
Description=EPAR Rosetta amd64 Linux support
After=local-fs.target systemd-binfmt.service
Before=docker.service actions-runner.service

[Service]
Type=oneshot
Environment=EPAR_ROSETTA_TAG=${rosetta_tag}
Environment=EPAR_ROSETTA_MOUNT=${rosetta_mount}
ExecStart=/opt/epar/setup-rosetta.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF_SERVICE

systemctl daemon-reload
systemctl enable epar-rosetta.service
EPAR_ROSETTA_TAG="${rosetta_tag}" EPAR_ROSETTA_MOUNT="${rosetta_mount}" /opt/epar/setup-rosetta.sh

touch /opt/epar/features/rosetta-amd64

if [[ -x /opt/epar/validate-rosetta-amd64.sh ]]; then
  bash /opt/epar/validate-rosetta-amd64.sh
fi

echo "Tart Rosetta amd64 support installed"
