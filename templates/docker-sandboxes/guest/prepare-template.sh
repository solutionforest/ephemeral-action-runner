#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "prepare-template.sh must run as root" >&2
  exit 1
fi

for command_name in bash cut docker dockerd dpkg-query find getent grep groupadd groupmod head install nohup pgrep ps readlink seq sha256sum sort stat sudo tar tr useradd usermod wc; do
  command -v "${command_name}" >/dev/null 2>&1 || {
    echo "pinned source image is missing required command: ${command_name}" >&2
    exit 1
  }
done

agent_uid="$(id -u agent 2>/dev/null || true)"
uid_1000_user="$(getent passwd 1000 | cut -d: -f1 || true)"

if [[ -n "${agent_uid}" && "${agent_uid}" != "1000" ]]; then
  echo "pinned source image already has an agent user with unexpected UID ${agent_uid}" >&2
  exit 1
fi

if [[ -z "${agent_uid}" ]]; then
	if [[ -n "${uid_1000_user}" && "${uid_1000_user}" != "ubuntu" && "${uid_1000_user}" != "packer" ]]; then
		echo "pinned source image assigns UID 1000 to unexpected user ${uid_1000_user}" >&2
		exit 1
	fi

	gid_1000_group="$(getent group 1000 | cut -d: -f1 || true)"
	if [[ -n "${gid_1000_group}" && "${gid_1000_group}" != "ubuntu" && "${gid_1000_group}" != "packer" && "${gid_1000_group}" != "agent" ]]; then
		echo "pinned source image assigns GID 1000 to unexpected group ${gid_1000_group}" >&2
		exit 1
	fi
	if [[ "${gid_1000_group}" == "ubuntu" || "${gid_1000_group}" == "packer" ]]; then
		groupmod --new-name agent "${gid_1000_group}"
	elif [[ -z "${gid_1000_group}" ]]; then
		groupadd --gid 1000 agent
	fi

	if [[ "${uid_1000_user}" == "ubuntu" || "${uid_1000_user}" == "packer" ]]; then
		usermod --login agent "${uid_1000_user}"
		usermod --home /home/agent --move-home agent
	else
    useradd --create-home --uid 1000 --gid 1000 --shell /bin/bash agent
  fi
fi

if [[ "$(id -u agent)" != "1000" || "$(id -g agent)" != "1000" ]]; then
  echo "agent identity must resolve to UID/GID 1000" >&2
  exit 1
fi
if [[ "$(getent passwd agent | cut -d: -f6)" != "/home/agent" ]]; then
  echo "agent home must resolve to /home/agent" >&2
  exit 1
fi

getent group docker >/dev/null 2>&1 || groupadd docker
getent group sudo >/dev/null 2>&1 || {
  echo "pinned source image is missing the sudo group" >&2
  exit 1
}
usermod --append --groups docker,sudo agent

# Never carry registry credentials from the pinned source image into a reusable
# runner template. Remove complete Docker client directories at the explicit
# source identities so symlinked or helper-backed configuration cannot survive.
rm -rf -- /root/.docker /home/runner/.docker /home/agent/.docker
for stale_docker_config in /root/.docker /home/runner/.docker /home/agent/.docker; do
  if [[ -e "${stale_docker_config}" || -L "${stale_docker_config}" ]]; then
    echo "failed to scrub source Docker client configuration at ${stale_docker_config}" >&2
    exit 1
  fi
done

install -d -m 0755 -o agent -g agent /home/agent
install -d -m 0700 -o agent -g agent \
  /home/agent/.docker \
  /home/agent/.docker/sandbox \
  /home/agent/.docker/sandbox/locks \
  /home/agent/.config \
  /home/agent/.cache \
  /home/agent/.local \
  /home/agent/.local/share \
  /home/agent/.local/state \
  /run/user/1000
install -d -m 0755 /etc/sudoers.d /etc/apt/apt.conf.d
printf '%s\n' 'agent ALL=(ALL:ALL) NOPASSWD:ALL' > /etc/sudoers.d/epar-agent
printf '%s\n' 'Defaults:agent env_keep += "http_proxy https_proxy no_proxy HTTP_PROXY HTTPS_PROXY NO_PROXY SSL_CERT_FILE NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE JAVA_TOOL_OPTIONS"' > /etc/sudoers.d/epar-proxy
chmod 0440 /etc/sudoers.d/epar-agent /etc/sudoers.d/epar-proxy
printf '%s\n' 'APT::Periodic::Enable "0";' 'APT::Periodic::Update-Package-Lists "0";' 'APT::Periodic::Unattended-Upgrade "0";' > /etc/apt/apt.conf.d/99epar-disable-periodic
rm -f /etc/systemd/system/timers.target.wants/apt-daily.timer /etc/systemd/system/timers.target.wants/apt-daily-upgrade.timer

# Docker Sandboxes supplies one private daemon and mounts its dedicated block
# volume here. Never preserve or preload a daemon data-root in the template.
rm -rf /var/lib/docker
install -d -m 0711 /var/lib/docker
if [[ -n "$(find /var/lib/docker -mindepth 1 -print -quit)" ]]; then
  echo "/var/lib/docker must be empty in the template" >&2
  exit 1
fi

sudo -u agent -H true
