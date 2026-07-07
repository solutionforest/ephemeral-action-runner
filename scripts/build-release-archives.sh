#!/usr/bin/env bash
set -euo pipefail

app="${APP_NAME:-ephemeral-action-runner}"
dist_dir="${DIST_DIR:-dist}"
version="${VERSION:-}"
commit="${COMMIT:-}"
build_date="${BUILD_DATE:-}"
go_cmd="${GO_CMD:-go}"
host_os="$(uname -s)"
host_arch="$(uname -m)"

if [[ -z "$version" ]]; then
  version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi
if [[ -z "$commit" ]]; then
  commit="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
fi
if [[ -z "$build_date" ]]; then
  build_date="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
fi

if ! command -v "$go_cmd" >/dev/null 2>&1; then
  if [[ "$go_cmd" == "go" ]] && command -v go.exe >/dev/null 2>&1; then
    go_cmd="go.exe"
  else
    echo "go command not found: $go_cmd" >&2
    echo "Set GO_CMD to the Go executable path if it is not named go on this shell PATH." >&2
    exit 1
  fi
fi

for required in README.md LICENSE configs scripts docs examples third_party/runner-images.lock; do
  if [[ ! -e "$required" ]]; then
    echo "missing required release input: $required" >&2
    exit 1
  fi
done

zip_package() {
  local source_dir="$1"
  local archive_path="$2"
  local package_root_dir="$3"
  local asset_name="$4"

  if command -v zip >/dev/null 2>&1; then
    (cd "$package_root_dir" && zip -qr "../${asset_name}.zip" "$asset_name")
    return
  fi

  if command -v powershell.exe >/dev/null 2>&1 && command -v cygpath >/dev/null 2>&1; then
    local source_win archive_win
    source_win="$(cygpath -w "$source_dir")"
    archive_win="$(cygpath -w "$archive_path")"
    EPAR_ZIP_SOURCE="$source_win" EPAR_ZIP_DEST="$archive_win" powershell.exe -NoProfile -NonInteractive -Command \
      'Compress-Archive -LiteralPath $env:EPAR_ZIP_SOURCE -DestinationPath $env:EPAR_ZIP_DEST -Force'
    return
  fi

  echo "zip command not found; install zip or run from a shell with powershell.exe and cygpath available" >&2
  exit 1
}

mkdir -p "$dist_dir"
package_root="$dist_dir/package"
rm -rf "$package_root"
mkdir -p "$package_root"

rm -f "$dist_dir"/"${app}"_*.tar "$dist_dir"/"${app}"_*.tar.gz "$dist_dir"/"${app}"_*.zip "$dist_dir/checksums.txt"

ldflags="-s -w -X main.version=${version} -X main.commit=${commit} -X main.buildDate=${build_date}"

targets=(
  "windows amd64 windows zip .exe"
  "linux amd64 linux tar.gz none"
  "linux arm64 linux tar.gz none"
  "darwin amd64 macos tar.gz none"
  "darwin arm64 macos tar.gz none"
)

for target in "${targets[@]}"; do
  read -r goos goarch asset_os archive_ext binary_ext <<<"$target"

  if [[ "$binary_ext" == "none" ]]; then
    binary_ext=""
  fi

  asset_name="${app}_${version}_${asset_os}_${goarch}"
  package_dir="$package_root/$asset_name"
  binary_name="${app}${binary_ext}"

  mkdir -p "$package_dir/third_party"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$go_cmd" build -trimpath -ldflags "$ldflags" -o "$package_dir/$binary_name" ./cmd/ephemeral-action-runner
  if [[ "$goos" != "windows" ]]; then
    chmod +x "$package_dir/$binary_name"
  fi
  cp README.md LICENSE "$package_dir/"
  cp -R configs scripts docs examples "$package_dir/"
  cp third_party/runner-images.lock "$package_dir/third_party/runner-images.lock"
  if [[ "$goos" == "windows" ]]; then
    cp scripts/release/run-epar.cmd "$package_dir/run-epar.cmd"
    cp scripts/release/run-epar.ps1 "$package_dir/run-epar.ps1"
  else
    cp scripts/release/run-epar "$package_dir/run-epar"
    chmod +x "$package_dir/run-epar"
  fi

  if [[ "$goos" == "linux" && "$goarch" == "amd64" && "$host_os" == "Linux" && "$host_arch" =~ ^(x86_64|amd64)$ ]]; then
    "$package_dir/$binary_name" version
  fi

  if [[ "$archive_ext" == "zip" ]]; then
    zip_package "$package_dir" "$dist_dir/${asset_name}.zip" "$package_root" "$asset_name"
  else
    tar_path="$dist_dir/${asset_name}.tar"
    rm -f "$tar_path" "$dist_dir/${asset_name}.${archive_ext}"
    tar -C "$package_root" --mode=755 -cf "$tar_path" "$asset_name/$binary_name" "$asset_name/run-epar"
    tar -C "$package_root" --exclude="$asset_name/$binary_name" --exclude="$asset_name/run-epar" -rf "$tar_path" "$asset_name"
    gzip -f "$tar_path"
  fi

  rm -rf "$package_dir"
done

rm -rf "$package_root"

(
  cd "$dist_dir"
  shopt -s nullglob
  archives=( *.tar.gz *.zip )
  if [[ "${#archives[@]}" -eq 0 ]]; then
    echo "no release archives were produced" >&2
    exit 1
  fi
  sha256sum "${archives[@]}" | sed 's/ \*/  /' | sort -k 2 > checksums.txt
)
ls -lh "$dist_dir"
