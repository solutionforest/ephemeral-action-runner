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
      "docs/usage.md" => "Usage",
      "usage.md" => "Usage",
      "docs/github-app.md" => "GitHub-App-Setup",
      "github-app.md" => "GitHub-App-Setup",
      "docs/image-build.md" => "Image-Build",
      "image-build.md" => "Image-Build",
      "docs/design.md" => "Design",
      "design.md" => "Design",
      "docs/operations.md" => "Operations",
      "operations.md" => "Operations",
      "docs/security.md" => "Security",
      "security.md" => "Security",
      "docs/background.md" => "Background",
      "background.md" => "Background",
      "docs/providers/tart.md" => "Tart-Provider",
      "providers/tart.md" => "Tart-Provider",
      "tart.md" => "Tart-Provider",
      "docs/providers/wsl.md" => "WSL-Provider",
      "providers/wsl.md" => "WSL-Provider",
      "wsl.md" => "WSL-Provider",
      "docs/providers/docker-dind.md" => "Docker-DinD-Provider",
      "providers/docker-dind.md" => "Docker-DinD-Provider",
      "docker-dind.md" => "Docker-DinD-Provider",
      "docs/providers/adding-provider.md" => "Adding-A-Provider",
      "providers/adding-provider.md" => "Adding-A-Provider",
      "adding-provider.md" => "Adding-A-Provider",
      "docs/advanced/docker-registry-mirrors.md" => "Docker-Registry-Mirrors",
      "advanced/docker-registry-mirrors.md" => "Docker-Registry-Mirrors",
      "docker-registry-mirrors.md" => "Docker-Registry-Mirrors",
      "docs/advanced/macos-startup.md" => "macOS-Startup",
      "advanced/macos-startup.md" => "macOS-Startup",
      "macos-startup.md" => "macOS-Startup",
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
copy_page "docs/usage.md" "Usage"
copy_page "docs/github-app.md" "GitHub-App-Setup"
copy_page "docs/image-build.md" "Image-Build"
copy_page "docs/design.md" "Design"
copy_page "docs/operations.md" "Operations"
copy_page "docs/security.md" "Security"
copy_page "docs/background.md" "Background"
copy_page "docs/providers/tart.md" "Tart-Provider"
copy_page "docs/providers/wsl.md" "WSL-Provider"
copy_page "docs/providers/docker-dind.md" "Docker-DinD-Provider"
copy_page "docs/providers/adding-provider.md" "Adding-A-Provider"
copy_page "docs/advanced/docker-registry-mirrors.md" "Docker-Registry-Mirrors"
copy_page "docs/advanced/macos-startup.md" "macOS-Startup"

for file in "$out_dir"/*.md; do
  rewrite_links "$file"
done

cat > "$out_dir/_Sidebar.md" <<'SIDEBAR'
# EPAR Docs

- [Home](Home)
- [Usage](Usage)
- [GitHub App Setup](GitHub-App-Setup)
- [Image Build](Image-Build)

## Providers

- [Docker-DinD](Docker-DinD-Provider)
- [Tart](Tart-Provider)
- [WSL](WSL-Provider)
- [Adding A Provider](Adding-A-Provider)

## Operations

- [Operations](Operations)
- [Security](Security)
- [Design](Design)
- [Background](Background)
- [Docker Registry Mirrors](Docker-Registry-Mirrors)
- [macOS Startup](macOS-Startup)
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
