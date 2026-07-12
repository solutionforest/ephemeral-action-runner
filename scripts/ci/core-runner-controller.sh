#!/usr/bin/env bash
set -euo pipefail

# This script is intentionally run only by the trusted controller job. The
# canary jobs never receive the GitHub App credentials used here.

api_required=(
  GITHUB_TOKEN
  GITHUB_REPOSITORY
  GITHUB_RUN_ID
  GITHUB_RUN_ATTEMPT
  RUNNER_TEMP
)
for name in "${api_required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    echo "::error::required environment variable ${name} is empty" >&2
    exit 1
  fi
done

github_token="${GITHUB_TOKEN}"
unset GITHUB_TOKEN
GITHUB_API_URL="${GITHUB_API_URL:-https://api.github.com}"
GITHUB_SERVER_URL="${GITHUB_SERVER_URL:-https://github.com}"
run_dir=""

cancel_during_initialization() {
  local status=$?
  trap - EXIT
  [[ -z "${run_dir}" ]] || rm -rf -- "${run_dir}"
  if (( status != 0 )) && command -v curl >/dev/null 2>&1; then
    curl --fail --silent --show-error \
      --request POST \
      --header "Authorization: Bearer ${github_token}" \
      --header "Accept: application/vnd.github+json" \
      --header "X-GitHub-Api-Version: 2022-11-28" \
      "${GITHUB_API_URL%/}/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/cancel" \
      >/dev/null 2>&1 || true
  fi
  exit "${status}"
}
trap cancel_during_initialization EXIT

required=(
  EPAR_BINARY
  EPAR_PROJECT_ROOT
  EPAR_APP_ID
  EPAR_ORGANIZATION
  EPAR_APP_PRIVATE_KEY
  CORE_CANARY_LABEL
)
for name in "${required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    echo "::error::required environment variable ${name} is empty" >&2
    exit 1
  fi
done

# Remove the private key from the exported environment before invoking any
# child process. It is retained only as a non-exported shell variable until it
# is materialized below.
app_private_key="${EPAR_APP_PRIVATE_KEY}"
unset EPAR_APP_PRIVATE_KEY

CORE_POOL_PREFIX="${CORE_POOL_PREFIX:-epar-ci-core}"
CORE_RUNNER_GROUP="${CORE_RUNNER_GROUP:-epar-ci-canary}"
CORE_MAX_WAIT_SECONDS="${CORE_MAX_WAIT_SECONDS:-2400}"
CORE_POLL_SECONDS="${CORE_POLL_SECONDS:-10}"
CORE_CLEANUP_MAX_ATTEMPTS="${CORE_CLEANUP_MAX_ATTEMPTS:-6}"
CORE_CLEANUP_TOTAL_SECONDS="${CORE_CLEANUP_TOTAL_SECONDS:-300}"
CORE_CLEANUP_ATTEMPT_SECONDS="${CORE_CLEANUP_ATTEMPT_SECONDS:-60}"
CORE_CLEANUP_SETTLE_SECONDS="${CORE_CLEANUP_SETTLE_SECONDS:-2}"
EPAR_SOURCE_IMAGE="${EPAR_SOURCE_IMAGE:-ghcr.io/catthehacker/ubuntu:act-latest@sha256:2362bb12b0c61438d334b9ed3686809981796a864ab89d93b5ee657652774eb7}"
EPAR_OUTPUT_IMAGE="${EPAR_OUTPUT_IMAGE:-epar-ci-core-image}"

if [[ ! "${EPAR_APP_ID}" =~ ^[0-9]+$ ]]; then
  echo "::error::EPAR_APP_ID must contain only digits" >&2
  exit 1
fi
if [[ ! "${EPAR_ORGANIZATION}" =~ ^[A-Za-z0-9_.-]+$ ]]; then
  echo "::error::EPAR_ORGANIZATION contains unsupported characters" >&2
  exit 1
fi
if [[ ! "${CORE_CANARY_LABEL}" =~ ^[A-Za-z0-9_.-]+$ ]]; then
  echo "::error::CORE_CANARY_LABEL contains unsupported characters" >&2
  exit 1
fi
if [[ ! "${CORE_POOL_PREFIX}" =~ ^[A-Za-z0-9_.-]+$ ]]; then
  echo "::error::CORE_POOL_PREFIX contains unsupported characters" >&2
  exit 1
fi
group_pattern='^[A-Za-z0-9_. -]+$'
if [[ ! "${CORE_RUNNER_GROUP}" =~ ${group_pattern} ]]; then
  echo "::error::CORE_RUNNER_GROUP contains unsupported characters" >&2
  exit 1
fi
if [[ ! "${CORE_MAX_WAIT_SECONDS}" =~ ^[0-9]+$ ]] || (( CORE_MAX_WAIT_SECONDS < 60 )); then
  echo "::error::CORE_MAX_WAIT_SECONDS must be an integer of at least 60" >&2
  exit 1
fi
if [[ ! "${CORE_POLL_SECONDS}" =~ ^[0-9]+$ ]] || (( CORE_POLL_SECONDS < 1 )); then
  echo "::error::CORE_POLL_SECONDS must be a positive integer" >&2
  exit 1
fi
if [[ ! "${CORE_CLEANUP_MAX_ATTEMPTS}" =~ ^[0-9]+$ ]] || (( CORE_CLEANUP_MAX_ATTEMPTS < 2 )); then
  echo "::error::CORE_CLEANUP_MAX_ATTEMPTS must be an integer of at least 2" >&2
  exit 1
fi
if [[ ! "${CORE_CLEANUP_TOTAL_SECONDS}" =~ ^[0-9]+$ ]] || (( CORE_CLEANUP_TOTAL_SECONDS < 1 )); then
  echo "::error::CORE_CLEANUP_TOTAL_SECONDS must be a positive integer" >&2
  exit 1
fi
if [[ ! "${CORE_CLEANUP_ATTEMPT_SECONDS}" =~ ^[0-9]+$ ]] || (( CORE_CLEANUP_ATTEMPT_SECONDS < 1 )); then
  echo "::error::CORE_CLEANUP_ATTEMPT_SECONDS must be a positive integer" >&2
  exit 1
fi
if [[ ! "${CORE_CLEANUP_SETTLE_SECONDS}" =~ ^[0-9]+$ ]]; then
  echo "::error::CORE_CLEANUP_SETTLE_SECONDS must be a non-negative integer" >&2
  exit 1
fi
if [[ -n "${EPAR_TRUSTED_CA_CERTIFICATE_PATH:-}" ]]; then
  if [[ "${EPAR_TRUSTED_CA_CERTIFICATE_PATH}" == *$'\n'* ||
        "${EPAR_TRUSTED_CA_CERTIFICATE_PATH}" == *$'\r'* ||
        "${EPAR_TRUSTED_CA_CERTIFICATE_PATH}" == *'"'* ||
        "${EPAR_TRUSTED_CA_CERTIFICATE_PATH}" == *'#'* ]]; then
    echo "::error::EPAR_TRUSTED_CA_CERTIFICATE_PATH cannot be represented safely in the CI config" >&2
    exit 1
  fi
  if [[ ! -r "${EPAR_TRUSTED_CA_CERTIFICATE_PATH}" ]]; then
    echo "::error::trusted CA certificate is not readable: ${EPAR_TRUSTED_CA_CERTIFICATE_PATH}" >&2
    exit 1
  fi
fi
if [[ -n "${EPAR_DOCKER_REGISTRY_MIRROR:-}" ]]; then
  if [[ "${EPAR_DOCKER_REGISTRY_MIRROR}" == *$'\n'* ||
        "${EPAR_DOCKER_REGISTRY_MIRROR}" == *$'\r'* ||
        "${EPAR_DOCKER_REGISTRY_MIRROR}" == *' '* ||
        "${EPAR_DOCKER_REGISTRY_MIRROR}" == *$'\t'* ||
        "${EPAR_DOCKER_REGISTRY_MIRROR}" == *'"'* ||
        "${EPAR_DOCKER_REGISTRY_MIRROR}" == *"'"* ||
        "${EPAR_DOCKER_REGISTRY_MIRROR}" == *'#'* ]]; then
    echo "::error::EPAR_DOCKER_REGISTRY_MIRROR contains unsupported characters" >&2
    exit 1
  fi
  case "${EPAR_DOCKER_REGISTRY_MIRROR}" in
    http://*|https://*) ;;
    *)
      echo "::error::EPAR_DOCKER_REGISTRY_MIRROR must be one HTTP(S) URL" >&2
      exit 1
      ;;
  esac
  mirror_authority="${EPAR_DOCKER_REGISTRY_MIRROR#*://}"
  mirror_authority="${mirror_authority%%/*}"
  mirror_host="${mirror_authority}"
  if [[ "${mirror_authority}" == *:* ]]; then
    mirror_port="${mirror_authority##*:}"
    mirror_host="${mirror_authority%:*}"
    if [[ ! "${mirror_port}" =~ ^[0-9]+$ ]] || (( mirror_port < 1 || mirror_port > 65535 )); then
      echo "::error::EPAR_DOCKER_REGISTRY_MIRROR has an invalid port" >&2
      exit 1
    fi
  fi
  mirror_host_pattern='^[A-Za-z0-9][A-Za-z0-9.-]*$'
  if [[ ! "${mirror_host}" =~ ${mirror_host_pattern} ]]; then
    echo "::error::EPAR_DOCKER_REGISTRY_MIRROR has an invalid host" >&2
    exit 1
  fi
fi
if [[ -n "${EPAR_DOCKER_PROXY:-}" ]]; then
  if [[ "${EPAR_DOCKER_PROXY}" == *$'\n'* ||
        "${EPAR_DOCKER_PROXY}" == *$'\r'* ||
        "${EPAR_DOCKER_PROXY}" == *' '* ||
        "${EPAR_DOCKER_PROXY}" == *$'\t'* ||
        "${EPAR_DOCKER_PROXY}" == *'"'* ||
        "${EPAR_DOCKER_PROXY}" == *"'"* ||
        "${EPAR_DOCKER_PROXY}" == *'#'* ||
        "${EPAR_DOCKER_PROXY}" == *'@'* ]]; then
    echo "::error::EPAR_DOCKER_PROXY contains unsupported characters or userinfo" >&2
    exit 1
  fi
  case "${EPAR_DOCKER_PROXY}" in
    http://*|https://*) ;;
    *)
      echo "::error::EPAR_DOCKER_PROXY must be one HTTP(S) URL" >&2
      exit 1
      ;;
  esac
  proxy_authority="${EPAR_DOCKER_PROXY#*://}"
  proxy_authority="${proxy_authority%%/*}"
  proxy_host="${proxy_authority}"
  if [[ "${proxy_authority}" == *:* ]]; then
    proxy_port="${proxy_authority##*:}"
    proxy_host="${proxy_authority%:*}"
    if [[ ! "${proxy_port}" =~ ^[0-9]+$ ]] || (( proxy_port < 1 || proxy_port > 65535 )); then
      echo "::error::EPAR_DOCKER_PROXY has an invalid port" >&2
      exit 1
    fi
  fi
  proxy_host_pattern='^[A-Za-z0-9][A-Za-z0-9.-]*$'
  if [[ ! "${proxy_host}" =~ ${proxy_host_pattern} ]]; then
    echo "::error::EPAR_DOCKER_PROXY has an invalid host" >&2
    exit 1
  fi
fi

for command in curl docker jq timeout; do
  if ! command -v "${command}" >/dev/null 2>&1; then
    echo "::error::the controller runner requires ${command}" >&2
    exit 1
  fi
done
if [[ ! -x "${EPAR_BINARY}" ]]; then
  echo "::error::EPAR_BINARY is not executable: ${EPAR_BINARY}" >&2
  exit 1
fi
docker info >/dev/null

run_dir="$(mktemp -d "${RUNNER_TEMP%/}/epar-core-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}.XXXXXX")"
key_path="${run_dir}/github-app.pem"
config_path="${run_dir}/config.yml"
log_dir="${run_dir}/logs"
pool_log="${run_dir}/pool-supervisor.log"
pool_pid=""
cleanup_started=false
cleanup_result=0
failure_diagnostics_emitted=false

umask 077
mkdir -p "${log_dir}"
printf '%s' "${app_private_key}" >"${key_path}"
unset app_private_key
# The initialization trap already protects this credential material if config
# generation fails before the full EPAR cleanup trap is installed below.

cat >"${config_path}" <<EOF
github:
  appId: ${EPAR_APP_ID}
  organization: ${EPAR_ORGANIZATION}
  privateKeyPath: ${key_path}
  apiBaseUrl: ${GITHUB_API_URL%/}
  webBaseUrl: ${GITHUB_SERVER_URL%/}

image:
  sourceType: docker-image
  sourceImage: ${EPAR_SOURCE_IMAGE}
  sourcePlatform: linux/amd64
  outputImage: ${EPAR_OUTPUT_IMAGE}
  upstreamDir: third_party/runner-images
  upstreamLock: third_party/runner-images.lock
  runnerVersion: latest
  customInstallScripts:
EOF
if [[ -n "${EPAR_TRUSTED_CA_CERTIFICATE_PATH:-}" ]]; then
  cat >>"${config_path}" <<EOF
  trustedCaCertificatePaths:
    - "${EPAR_TRUSTED_CA_CERTIFICATE_PATH}"
EOF
fi
cat >>"${config_path}" <<EOF
pool:
  instances: 1
  namePrefix: ${CORE_POOL_PREFIX}
  logDir: ${log_dir}

runner:
  labels: [${CORE_CANARY_LABEL}]
  group: ${CORE_RUNNER_GROUP}
  noDefaultLabels: true
  includeHostLabel: false
  ephemeral: true

provider:
  type: docker-dind
  sourceImage: ${EPAR_OUTPUT_IMAGE}
  platform: linux/amd64
  network: default

docker:
  registryMirrors:
EOF
if [[ -n "${EPAR_DOCKER_REGISTRY_MIRROR:-}" ]]; then
  cat >>"${config_path}" <<EOF
    - "${EPAR_DOCKER_REGISTRY_MIRROR}"
EOF
fi
if [[ -n "${EPAR_DOCKER_PROXY:-}" ]]; then
  cat >>"${config_path}" <<EOF
  httpProxy: "${EPAR_DOCKER_PROXY}"
  httpsProxy: "${EPAR_DOCKER_PROXY}"
  noProxy: localhost,127.0.0.1,::1
EOF
fi
cat >>"${config_path}" <<EOF
timeouts:
  bootSeconds: 180
  githubOnlineSeconds: 180
  commandSeconds: 900
EOF

epar() {
  "${EPAR_BINARY}" "$@" --config "${config_path}" --project-root "${EPAR_PROJECT_ROOT}"
}

stop_pool() {
  if [[ -z "${pool_pid}" ]] || ! kill -0 "${pool_pid}" 2>/dev/null; then
    return
  fi

  kill -TERM "${pool_pid}" 2>/dev/null || true
  local deadline=$((SECONDS + 180))
  while kill -0 "${pool_pid}" 2>/dev/null && (( SECONDS < deadline )); do
    sleep 2
  done
  if kill -0 "${pool_pid}" 2>/dev/null; then
    echo "::warning::pool supervisor did not exit within 180 seconds; terminating it forcibly" >&2
    kill -KILL "${pool_pid}" 2>/dev/null || true
  fi
  wait "${pool_pid}" 2>/dev/null || true
  pool_pid=""
}

sanitize_diagnostic_stream() {
  awk '
    BEGIN { in_private_key = 0 }
    in_private_key {
      if ($0 ~ /-----END .*PRIVATE KEY-----/) {
        in_private_key = 0
      }
      next
    }
    /-----BEGIN .*PRIVATE KEY-----/ {
      print "[REDACTED PRIVATE KEY]"
      in_private_key = 1
      next
    }
    { print }
  ' | sed -E \
    -e 's/(Authorization:[[:space:]]*(Basic|Bearer|token)[[:space:]]+)[^[:space:]]+/\1***/Ig' \
    -e 's/(--token(=|[[:space:]]+))[^[:space:]]+/\1***/Ig' \
    -e 's/("[^"]*(TOKEN|SECRET|PASSWORD|PRIVATE[ _-]*KEY)[^"]*"[[:space:]]*:[[:space:]]*")([^"\\]|\\.)*/\1***/Ig' \
    -e 's/(([[:alnum:]_-]*(TOKEN|SECRET|PASSWORD|PRIVATE[_-]?KEY)[[:alnum:]_-]*)=)[^[:space:]]+/\1***/Ig' \
    -e 's/(^|[^[:alnum:]_])(gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})/\1***/Ig' \
    -e 's/(^|[^[:alnum:]_-])(eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,})/\1***/g' \
    -e 's/^[A-Za-z0-9+\/=]{40,}$/[REDACTED POSSIBLE KEY MATERIAL]/' \
    -e 's/^/| /'
}

print_sanitized_log_file() {
  local path="$1"
  local title="$2"
  if [[ ! -s "${path}" ]]; then
    return
  fi

  printf '::group::%s (last 200 lines, sanitized)\n' "${title}"
  tail -n 200 "${path}" | sanitize_diagnostic_stream || true
  echo '::endgroup::'
}

emit_failure_diagnostics() {
  if [[ "${failure_diagnostics_emitted}" == "true" ]]; then
    return
  fi
  failure_diagnostics_emitted=true

  print_sanitized_log_file "${pool_log}" 'EPAR pool supervisor log'

  local log_path
  local -a runner_logs=()
  shopt -s nullglob
  runner_logs=("${log_dir}"/*.log)
  shopt -u nullglob
  for log_path in "${runner_logs[@]}"; do
    print_sanitized_log_file "${log_path}" "EPAR runner log: $(basename "${log_path}")"
  done
}

cleanup() {
  if [[ "${cleanup_started}" == "true" ]]; then
    return "${cleanup_result}"
  fi
  cleanup_started=true
  set +e

  stop_pool

  local attempt cleanup_status container_name consecutive_empty=0
  local cleanup_deadline remaining_seconds attempt_seconds sleep_seconds
  local cleanup_converged=false
  local -a stale_containers=()
  cleanup_deadline=$((SECONDS + CORE_CLEANUP_TOTAL_SECONDS))
  for (( attempt = 1; attempt <= CORE_CLEANUP_MAX_ATTEMPTS; attempt++ )); do
    remaining_seconds=$((cleanup_deadline - SECONDS))
    if (( remaining_seconds <= 0 )); then
      echo "::warning::EPAR cleanup exhausted its ${CORE_CLEANUP_TOTAL_SECONDS}-second total budget before attempt ${attempt}" >&2
      break
    fi
    attempt_seconds="${CORE_CLEANUP_ATTEMPT_SECONDS}"
    if (( attempt_seconds > remaining_seconds )); then
      attempt_seconds="${remaining_seconds}"
    fi

    timeout "${attempt_seconds}" "${EPAR_BINARY}" cleanup \
      --config "${config_path}" \
      --project-root "${EPAR_PROJECT_ROOT}"
    cleanup_status=$?
    if (( cleanup_status != 0 )); then
      echo "::warning::EPAR cleanup attempt ${attempt}/${CORE_CLEANUP_MAX_ATTEMPTS} exited with status ${cleanup_status}" >&2
      consecutive_empty=0
    else
      stale_containers=()
      while IFS= read -r container_name; do
        case "${container_name}" in
          "${CORE_POOL_PREFIX}"|"${CORE_POOL_PREFIX}"-*) stale_containers+=("${container_name}") ;;
        esac
      done < <(docker ps --all --format '{{.Names}}')

      if (( ${#stale_containers[@]} == 0 )); then
        ((consecutive_empty++))
        if (( consecutive_empty >= 2 )); then
          cleanup_converged=true
          break
        fi
        echo "EPAR cleanup attempt ${attempt}/${CORE_CLEANUP_MAX_ATTEMPTS} found no in-boundary containers; confirming cleanup convergence."
      else
        consecutive_empty=0
        echo "::warning::Docker containers remain inside the ${CORE_POOL_PREFIX} cleanup boundary after attempt ${attempt}/${CORE_CLEANUP_MAX_ATTEMPTS}: ${stale_containers[*]}" >&2
      fi
    fi

    if (( attempt < CORE_CLEANUP_MAX_ATTEMPTS )); then
      remaining_seconds=$((cleanup_deadline - SECONDS))
      if (( remaining_seconds <= 0 )); then
        echo "::warning::EPAR cleanup exhausted its ${CORE_CLEANUP_TOTAL_SECONDS}-second total budget after attempt ${attempt}" >&2
        break
      fi
      sleep_seconds="${CORE_CLEANUP_SETTLE_SECONDS}"
      if (( sleep_seconds > remaining_seconds )); then
        sleep_seconds="${remaining_seconds}"
      fi
      sleep "${sleep_seconds}"
    fi
  done
  if [[ "${cleanup_converged}" != "true" ]]; then
    echo "::warning::EPAR cleanup did not converge within ${CORE_CLEANUP_MAX_ATTEMPTS} attempts and ${CORE_CLEANUP_TOTAL_SECONDS} seconds; containers may remain inside the ${CORE_POOL_PREFIX} cleanup boundary" >&2
    cleanup_result=1
  fi

  rm -f "${key_path}" "${config_path}"
  # The remaining directory contains logs only. It is removed so a persistent
  # controller runner cannot accumulate per-run data indefinitely.
  rm -rf "${run_dir}"
  set -e
  return "${cleanup_result}"
}

on_signal() {
  exit 130
}
trap on_signal INT TERM

api() {
  local method="$1"
  local path="$2"
  curl --fail --silent --show-error \
    --request "${method}" \
    --header "Authorization: Bearer ${github_token}" \
    --header "Accept: application/vnd.github+json" \
    --header "X-GitHub-Api-Version: 2022-11-28" \
    "${GITHUB_API_URL%/}${path}"
}

cancel_workflow() {
  workflow_cancel_requested=true
  echo "Cancelling workflow run ${GITHUB_RUN_ID} after controller failure."
  api POST "/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/cancel" >/dev/null || {
    echo "::warning::normal workflow cancellation failed; attempting force cancellation" >&2
    api POST "/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/force-cancel" >/dev/null || true
  }
}

workflow_cancel_requested=false
finalize() {
  local status=$?
  trap - EXIT
  if (( status != 0 )); then
    emit_failure_diagnostics || true
  fi
  if ! cleanup && (( status == 0 )); then
    status=1
  fi
  if (( status != 0 )) && [[ "${workflow_cancel_requested}" != "true" ]]; then
    cancel_workflow || true
  fi
  exit "${status}"
}
trap finalize EXIT

fail_run() {
  local message="$1"
  echo "::error::${message}" >&2
  emit_failure_diagnostics || true
  cleanup || true
  cancel_workflow
  exit 1
}

echo "Pre-cleaning stale ${CORE_POOL_PREFIX} runners and containers."
epar cleanup

echo "Building the pinned Level 1 core image."
epar image build --replace

echo "Starting one supervised ephemeral runner for label ${CORE_CANARY_LABEL}."
# Do not background the epar() shell function: Bash would track the function's
# wrapper process, while the EPAR supervisor it starts could outlive that
# wrapper. exec makes pool_pid the real EPAR supervisor process.
(
  exec "${EPAR_BINARY}" pool up --instances 1 --monitor-interval 5s \
    --config "${config_path}" \
    --project-root "${EPAR_PROJECT_ROOT}"
) >"${pool_log}" 2>&1 &
pool_pid=$!

deadline=$((SECONDS + CORE_MAX_WAIT_SECONDS))
last_report=""
while (( SECONDS < deadline )); do
  if ! kill -0 "${pool_pid}" 2>/dev/null; then
    wait "${pool_pid}" || pool_status=$?
    pool_status="${pool_status:-0}"
    fail_run "EPAR pool supervisor exited unexpectedly with status ${pool_status}"
  fi

  if ! jobs_json="$(api GET "/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/jobs?filter=latest&per_page=100")"; then
    echo "::warning::could not read workflow jobs; retrying" >&2
    sleep "${CORE_POLL_SECONDS}"
    continue
  fi

  job1="$(jq -c '[.jobs[] | select(.name == "Core canary 1")] | last // empty' <<<"${jobs_json}")"
  job2="$(jq -c '[.jobs[] | select(.name == "Core canary 2")] | last // empty' <<<"${jobs_json}")"
  job1_status="$(jq -r '.status // "missing"' <<<"${job1:-null}")"
  job2_status="$(jq -r '.status // "missing"' <<<"${job2:-null}")"
  job1_conclusion="$(jq -r '.conclusion // "-"' <<<"${job1:-null}")"
  job2_conclusion="$(jq -r '.conclusion // "-"' <<<"${job2:-null}")"
  report="canary1=${job1_status}/${job1_conclusion} canary2=${job2_status}/${job2_conclusion}"
  if [[ "${report}" != "${last_report}" ]]; then
    echo "${report}"
    last_report="${report}"
  fi

  if [[ "${job1_status}" == "completed" && "${job1_conclusion}" != "success" ]]; then
    fail_run "Core canary 1 completed with conclusion ${job1_conclusion}"
  fi
  if [[ "${job2_status}" == "completed" && "${job2_conclusion}" != "success" ]]; then
    fail_run "Core canary 2 completed with conclusion ${job2_conclusion}"
  fi

  if [[ "${job1_conclusion}" == "success" && "${job2_conclusion}" == "success" ]]; then
    runner1="$(jq -r '.runner_name // empty' <<<"${job1}")"
    runner2="$(jq -r '.runner_name // empty' <<<"${job2}")"
    group1="$(jq -r '.runner_group_name // empty' <<<"${job1}")"
    group2="$(jq -r '.runner_group_name // empty' <<<"${job2}")"

    [[ -n "${runner1}" && -n "${runner2}" ]] || fail_run "GitHub did not report both canary runner names"
    [[ "${runner1}" != "${runner2}" ]] || fail_run "Both canaries ran on ${runner1}; ephemeral replacement was not proven"
    [[ "${runner1}" == "${CORE_POOL_PREFIX}-"* && "${runner2}" == "${CORE_POOL_PREFIX}-"* ]] || \
      fail_run "Canary runner names were outside the ${CORE_POOL_PREFIX} cleanup boundary"
    [[ "${group1}" == "${CORE_RUNNER_GROUP}" && "${group2}" == "${CORE_RUNNER_GROUP}" ]] || \
      fail_run "Canaries did not report runner group ${CORE_RUNNER_GROUP}"
    jq -e --arg target_label "${CORE_CANARY_LABEL}" '.labels | index($target_label) != null' <<<"${job1}" >/dev/null || \
      fail_run "Core canary 1 did not report label ${CORE_CANARY_LABEL}"
    jq -e --arg target_label "${CORE_CANARY_LABEL}" '.labels | index($target_label) != null' <<<"${job2}" >/dev/null || \
      fail_run "Core canary 2 did not report label ${CORE_CANARY_LABEL}"

    {
      echo "### Level 1 core runner verification"
      echo
      echo "- Canary 1 runner: \`${runner1}\`"
      echo "- Canary 2 runner: \`${runner2}\`"
      echo "- Runner group: \`${CORE_RUNNER_GROUP}\`"
      echo "- Unique label: \`${CORE_CANARY_LABEL}\`"
    } >>"${GITHUB_STEP_SUMMARY}"
    echo "Both canaries passed on distinct ephemeral runners."
    if ! cleanup; then
      fail_run "Post-run cleanup left EPAR resources behind"
    fi
    exit 0
  fi

  sleep "${CORE_POLL_SECONDS}"
done

fail_run "Timed out after ${CORE_MAX_WAIT_SECONDS} seconds waiting for both canaries"
