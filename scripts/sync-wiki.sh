#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <output-dir>" >&2
}

if [ "$#" -ne 1 ]; then
  usage
  exit 2
fi

repo_root="$(git rev-parse --show-toplevel)"
out_dir="$1"

if [ -z "$out_dir" ] || [ "$out_dir" = "/" ] || [ "$out_dir" = "." ] || [ "$out_dir" = "$repo_root" ] || [ "$out_dir" = "$repo_root/" ]; then
  echo "refusing unsafe output directory: $out_dir" >&2
  exit 2
fi

rm -rf "$out_dir"
mkdir -p "$out_dir"

copy_assets() {
  if [ -d "$repo_root/docs/assets" ]; then
    mkdir -p "$out_dir/assets"
    cp -R "$repo_root/docs/assets/." "$out_dir/assets/"
  fi
  if [ -d "$repo_root/configs" ]; then
    mkdir -p "$out_dir/configs"
    cp -R "$repo_root/configs/." "$out_dir/configs/"
  fi
  if [ -f "$repo_root/templates/docker-sandboxes/sources.lock.json" ]; then
    mkdir -p "$out_dir/templates/docker-sandboxes"
    cp "$repo_root/templates/docker-sandboxes/sources.lock.json" "$out_dir/templates/docker-sandboxes/"
  fi
}

copy_page() {
  local source="$1"
  local target="$2"

  {
    printf '<!-- Generated from %s. Edit the main repository docs, not the wiki copy. -->\n\n' "$source"
    sed '/^<!-- Generated from .* -->$/d' "$repo_root/$source"
  } > "$out_dir/$target.md"
}

rewrite_links() {
  local file="$1"

  perl -0pi -e '
    my %map = (
      "README.md" => "Home",
      "docs/README.md" => "Documentation",
      "docs/usage.md" => "Usage",
      "usage.md" => "Usage",
      "docs/configuration.md" => "Configuration",
      "configuration.md" => "Configuration",
      "docs/github-app.md" => "GitHub-App-Setup",
      "github-app.md" => "GitHub-App-Setup",
      "docs/runner-groups.md" => "Runner-Groups",
      "runner-groups.md" => "Runner-Groups",
      "docs/image-build.md" => "Image-Build",
      "image-build.md" => "Image-Build",
      "docs/development/design.md" => "Design",
      "development/design.md" => "Design",
      "design.md" => "Design",
      "docs/development/principles.md" => "Development-Principles",
      "development/principles.md" => "Development-Principles",
      "principles.md" => "Development-Principles",
      "docs/operations.md" => "Operations",
      "operations.md" => "Operations",
      "docs/logging.md" => "Logging",
      "logging.md" => "Logging",
      "docs/storage.md" => "Storage",
      "storage.md" => "Storage",
      "docs/generated-files.md" => "Generated-Files-and-Recovery",
      "generated-files.md" => "Generated-Files-and-Recovery",
      "docs/troubleshooting.md" => "Troubleshooting",
      "troubleshooting.md" => "Troubleshooting",
      "docs/security.md" => "Security",
      "security.md" => "Security",
      "docs/providers/tart.md" => "Tart-Provider",
      "providers/tart.md" => "Tart-Provider",
      "tart.md" => "Tart-Provider",
      "docs/providers/wsl.md" => "WSL-Provider",
      "providers/wsl.md" => "WSL-Provider",
      "wsl.md" => "WSL-Provider",
      "docs/providers/docker-container.md" => "Docker-Container-Provider",
      "providers/docker-container.md" => "Docker-Container-Provider",
      "docker-container.md" => "Docker-Container-Provider",
      "docs/providers/docker-sandboxes.md" => "Docker-Sandboxes-Provider",
      "providers/docker-sandboxes.md" => "Docker-Sandboxes-Provider",
      "docker-sandboxes.md" => "Docker-Sandboxes-Provider",
      "docs/development/adding-provider.md" => "Adding-A-Provider",
      "development/adding-provider.md" => "Adding-A-Provider",
      "adding-provider.md" => "Adding-A-Provider",
      "docs/advanced/docker-registry-mirrors.md" => "Docker-Registry-Mirrors",
      "advanced/docker-registry-mirrors.md" => "Docker-Registry-Mirrors",
      "docker-registry-mirrors.md" => "Docker-Registry-Mirrors",
      "docs/advanced/docker-sandboxes-template.md" => "Docker-Sandboxes-Template",
      "advanced/docker-sandboxes-template.md" => "Docker-Sandboxes-Template",
      "docker-sandboxes-template.md" => "Docker-Sandboxes-Template",
      "docs/advanced/cross-architecture-containers.md" => "Cross-Architecture-Containers",
      "advanced/cross-architecture-containers.md" => "Cross-Architecture-Containers",
      "cross-architecture-containers.md" => "Cross-Architecture-Containers",
      "docs/advanced/no-go-install.md" => "No-Go-Install",
      "advanced/no-go-install.md" => "No-Go-Install",
      "no-go-install.md" => "No-Go-Install",
      "docs/advanced/windows-startup.md" => "Windows-Startup",
      "advanced/windows-startup.md" => "Windows-Startup",
      "windows-startup.md" => "Windows-Startup",
      "docs/advanced/macos-startup.md" => "macOS-Startup",
      "advanced/macos-startup.md" => "macOS-Startup",
      "macos-startup.md" => "macOS-Startup",
      "docs/development/README.md" => "Development",
      "development/README.md" => "Development",
      "docs/development/" => "Development",
      "development/" => "Development",
      "" => "Documentation",
      "docs/development/core-runner-verification.md" => "Core-Runner-Verification",
      "development/core-runner-verification.md" => "Core-Runner-Verification",
      "core-runner-verification.md" => "Core-Runner-Verification",
      "docs/development/releases.md" => "Releases",
      "development/releases.md" => "Releases",
      "releases.md" => "Releases",
      "examples/observability/README.md" => "Observability-Examples",
      "observability/README.md" => "Observability-Examples",
      "SUPPORT.md" => "Support",
      "CONTRIBUTING.md" => "Contributing",
      "CODE_OF_CONDUCT.md" => "Code-of-Conduct",
    );

    s{\]\(([^)]+)\)}{
      my $raw = $1;
      if ($raw =~ m{^(?:https?://|mailto:|#)}) {
        "]($raw)";
      } else {
        my ($path, $anchor) = $raw =~ /^([^#]*)(#.*)?$/;
        $anchor //= "";
        $path =~ s{^\./}{};
        while ($path =~ s{^\.\./}{}) {}
        if ($path =~ s{^docs/assets/}{assets/}) {
          "]($path$anchor)";
        } elsif ($path =~ m{^(?:configs|templates)/}) {
          "]($path$anchor)";
        } elsif (exists $map{$path}) {
          "]($map{$path}$anchor)";
        } else {
          "]($raw)";
        }
      }
    }eg;
  ' "$file"
}

copy_assets

copy_page "README.md" "Home"
copy_page "docs/README.md" "Documentation"
copy_page "docs/usage.md" "Usage"
copy_page "docs/configuration.md" "Configuration"
copy_page "docs/github-app.md" "GitHub-App-Setup"
copy_page "docs/runner-groups.md" "Runner-Groups"
copy_page "docs/image-build.md" "Image-Build"
copy_page "docs/development/design.md" "Design"
copy_page "docs/development/principles.md" "Development-Principles"
copy_page "docs/operations.md" "Operations"
copy_page "docs/logging.md" "Logging"
copy_page "docs/storage.md" "Storage"
copy_page "docs/generated-files.md" "Generated-Files-and-Recovery"
copy_page "docs/troubleshooting.md" "Troubleshooting"
copy_page "docs/security.md" "Security"
copy_page "docs/providers/docker-sandboxes.md" "Docker-Sandboxes-Provider"
copy_page "docs/providers/docker-container.md" "Docker-Container-Provider"
copy_page "docs/providers/wsl.md" "WSL-Provider"
copy_page "docs/providers/tart.md" "Tart-Provider"
copy_page "docs/development/adding-provider.md" "Adding-A-Provider"
copy_page "docs/advanced/docker-registry-mirrors.md" "Docker-Registry-Mirrors"
copy_page "docs/advanced/docker-sandboxes-template.md" "Docker-Sandboxes-Template"
copy_page "docs/advanced/cross-architecture-containers.md" "Cross-Architecture-Containers"
copy_page "docs/advanced/no-go-install.md" "No-Go-Install"
copy_page "docs/advanced/windows-startup.md" "Windows-Startup"
copy_page "docs/advanced/macos-startup.md" "macOS-Startup"
copy_page "docs/development/README.md" "Development"
copy_page "docs/development/core-runner-verification.md" "Core-Runner-Verification"
copy_page "docs/development/releases.md" "Releases"
copy_page "examples/observability/README.md" "Observability-Examples"
copy_page "SUPPORT.md" "Support"
copy_page "CONTRIBUTING.md" "Contributing"
copy_page "CODE_OF_CONDUCT.md" "Code-of-Conduct"

for file in "$out_dir"/*.md; do
  rewrite_links "$file"
done

cat > "$out_dir/_Sidebar.md" <<'SIDEBAR'
# EPAR Docs

- [Home](Home)
- [Documentation](Documentation)
- [Usage](Usage)
- [Configuration](Configuration)
- [GitHub App Setup](GitHub-App-Setup)
- [Image Build](Image-Build)

## Providers

- [Docker Sandboxes](Docker-Sandboxes-Provider)

## Compatibility providers

- [Docker Container](Docker-Container-Provider)
- [WSL2](WSL-Provider)
- [Tart (retired)](Tart-Provider)
- [Adding A Provider](Adding-A-Provider)

## Operations

- [Operations](Operations)
- [Logging](Logging)
- [Observability Examples](Observability-Examples)
- [Storage](Storage)
- [Generated files and recovery](Generated-Files-and-Recovery)
- [Troubleshooting](Troubleshooting)
- [Security](Security)

## Advanced

- [Docker Sandboxes Template](Docker-Sandboxes-Template)
- [Cross-Architecture Containers](Cross-Architecture-Containers)
- [Docker Registry Mirrors](Docker-Registry-Mirrors)
- [No-Go Installation](No-Go-Install)
- [Windows Startup](Windows-Startup)
- [macOS Startup](macOS-Startup)

## Development

- [Development Guide](Development)
- [Design](Design)
- [Development Principles](Development-Principles)
- [Core Runner Verification](Core-Runner-Verification)
- [Releases](Releases)
- [Contributing](Contributing)
- [Support](Support)
SIDEBAR

source_ref="${GITHUB_SHA:-}"
if [ -n "$source_ref" ]; then
  source_ref="${source_ref:0:7}"
else
  source_ref="$(git -C "$repo_root" rev-parse --short HEAD 2>/dev/null || echo unknown)"
fi

cat > "$out_dir/_Footer.md" <<FOOTER
Generated from the main repository docs at \`$source_ref\`. Edit \`README.md\` and \`docs/\`; the wiki copy is overwritten by automation.
FOOTER
