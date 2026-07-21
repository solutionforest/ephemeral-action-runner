#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d)"
cleanup_test_root() {
  if [[ -f "${test_root}/pool.pid" ]]; then
    pool_pid="$(<"${test_root}/pool.pid")"
    [[ "${pool_pid}" =~ ^[0-9]+$ ]] && kill -KILL "${pool_pid}" 2>/dev/null || true
  fi
  rm -rf "${test_root}"
}
trap cleanup_test_root EXIT
mkdir -p "${test_root}/bin" "${test_root}/runner-temp"
printf '%s\n' 'test inspection CA' >"${test_root}/inspection-ca.pem"

workflow_path="${repo_root}/.github/workflows/core-runner-verification.yml"
grep -F -q 'if: ${{ failure() && !cancelled() }}' "${workflow_path}"
grep -F -q 'Cancel workflow after controller setup failure' "${workflow_path}"
grep -F -q '"${api_url}/force-cancel"' "${workflow_path}"
grep -F -q 'runs-on: ubuntu-latest' "${workflow_path}"

cat >"${test_root}/bin/docker" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == "ps" ]]; then
  scan_count=0
  if [[ -f "${MOCK_DOCKER_PS_COUNT}" ]]; then
    scan_count="$(<"${MOCK_DOCKER_PS_COUNT}")"
  fi
  scan_count=$((scan_count + 1))
  printf '%s\n' "${scan_count}" >"${MOCK_DOCKER_PS_COUNT}"
  if (( scan_count <= ${MOCK_DOCKER_LATE_RESOURCE_SCANS:-0} )); then
    printf '%s\n' 'epar-ci-core-late-resource'
  elif (( scan_count <= ${MOCK_DOCKER_LATE_RESOURCE_SCANS:-0} + ${MOCK_DOCKER_OUTSIDE_BOUNDARY_SCANS:-0} )); then
    printf '%s\n' 'epar-ci-corex-outside-boundary'
  fi
fi
exit 0
EOF

cat >"${test_root}/bin/curl" <<'EOF'
#!/usr/bin/env bash
if [[ " $* " == *" --request GET "* ]]; then
  if [[ "${MOCK_POOL_FAILURE:-}" == "1" ]]; then
    cat <<'JSON'
{"jobs":[
  {"name":"Core canary 1","status":"queued","conclusion":null,"runner_name":null,"runner_group_name":null,"labels":["epar-core-123-1"]},
  {"name":"Core canary 2","status":"queued","conclusion":null,"runner_name":null,"runner_group_name":null,"labels":["epar-core-123-1"]}
]}
JSON
  else
    cat <<'JSON'
{"jobs":[
  {"name":"Core canary 1","status":"completed","conclusion":"success","runner_name":"epar-ci-core-20260710-100000-001","runner_group_name":"epar-ci-canary","labels":["epar-core-123-1"]},
  {"name":"Core canary 2","status":"completed","conclusion":"success","runner_name":"epar-ci-core-20260710-100010-002","runner_group_name":"epar-ci-canary","labels":["epar-core-123-1"]}
]}
JSON
  fi
else
  : >"${MOCK_CANCEL_MARKER}"
fi
EOF

cat >"${test_root}/bin/jq" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

raw=false
target_label=""
while (($#)); do
  case "$1" in
    -c) shift ;;
    -r) raw=true; shift ;;
    -e) shift ;;
    --arg)
      [[ "${2:-}" == "target_label" ]]
      target_label="${3:-}"
      shift 3
      ;;
    *) break ;;
  esac
done
expr="${1:?jq expression is required}"
input="$(cat)"

case "${expr}" in
  *'Core canary 1'*)
    if [[ "${input}" == *'"status":"queued"'* ]]; then
      printf '%s\n' '{"name":"Core canary 1","status":"queued","conclusion":null,"runner_name":null,"runner_group_name":null,"labels":["epar-core-123-1"]}'
    else
      printf '%s\n' '{"name":"Core canary 1","status":"completed","conclusion":"success","runner_name":"epar-ci-core-20260710-100000-001","runner_group_name":"epar-ci-canary","labels":["epar-core-123-1"]}'
    fi
    ;;
  *'Core canary 2'*)
    if [[ "${input}" == *'"status":"queued"'* ]]; then
      printf '%s\n' '{"name":"Core canary 2","status":"queued","conclusion":null,"runner_name":null,"runner_group_name":null,"labels":["epar-core-123-1"]}'
    else
      printf '%s\n' '{"name":"Core canary 2","status":"completed","conclusion":"success","runner_name":"epar-ci-core-20260710-100010-002","runner_group_name":"epar-ci-canary","labels":["epar-core-123-1"]}'
    fi
    ;;
  '.status // "missing"')
    sed -n 's/.*"status":"\([^"]*\)".*/\1/p' <<<"${input}"
    ;;
  '.conclusion // "-"')
    sed -n 's/.*"conclusion":"\([^"]*\)".*/\1/p' <<<"${input}"
    ;;
  '.runner_name // empty')
    sed -n 's/.*"runner_name":"\([^"]*\)".*/\1/p' <<<"${input}"
    ;;
  '.runner_group_name // empty')
    sed -n 's/.*"runner_group_name":"\([^"]*\)".*/\1/p' <<<"${input}"
    ;;
  '.labels | index($target_label) != null')
    if [[ "${input}" == *"\"${target_label}\""* ]]; then
      [[ "${raw}" == "true" ]] && printf 'true\n'
      exit 0
    fi
    [[ "${raw}" == "true" ]] && printf 'false\n'
    exit 1
    ;;
  *)
    echo "unsupported mock jq expression: ${expr}" >&2
    exit 2
    ;;
esac
EOF

cat >"${test_root}/bin/epar" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_EPAR_LOG}"
if [[ "${1:-}" == "cleanup" && -n "${MOCK_CLEANUP_CALL_COUNT_FILE:-}" ]]; then
  cleanup_calls=0
  if [[ -f "${MOCK_CLEANUP_CALL_COUNT_FILE}" ]]; then
    cleanup_calls="$(<"${MOCK_CLEANUP_CALL_COUNT_FILE}")"
  fi
  cleanup_calls=$((cleanup_calls + 1))
  printf '%s\n' "${cleanup_calls}" >"${MOCK_CLEANUP_CALL_COUNT_FILE}"
  if [[ "${MOCK_CLEANUP_FAIL_ON_CALL:-}" == "${cleanup_calls}" ]]; then
    : >"${MOCK_CLEANUP_FAIL_MARKER}"
    exit "${MOCK_CLEANUP_FAIL_STATUS:-7}"
  fi
fi
if [[ " $* " == *" image build "* ]]; then
  config_path=""
  while (( $# > 0 )); do
    if [[ "$1" == "--config" ]]; then
      config_path="$2"
      break
    fi
    shift
  done
  grep -F -q 'trustedCaCertificatePaths:' "${config_path}"
  grep -F -q "${MOCK_CA_PATH}" "${config_path}"
  grep -F -q 'registryMirrors:' "${config_path}"
  grep -F -q "${MOCK_REGISTRY_MIRROR}" "${config_path}"
  grep -F -q "httpProxy: \"${MOCK_DOCKER_PROXY}\"" "${config_path}"
  grep -F -q "httpsProxy: \"${MOCK_DOCKER_PROXY}\"" "${config_path}"
  grep -F -q 'noProxy: localhost,127.0.0.1,::1' "${config_path}"
fi
if [[ " $* " == *" pool up "* ]]; then
  if [[ "${MOCK_POOL_FAILURE:-}" == "1" ]]; then
    config_path=""
    while (( $# > 0 )); do
      if [[ "$1" == "--config" ]]; then
        config_path="$2"
        break
      fi
      shift
    done
    log_dir="$(sed -n 's/^  directory: //p' "${config_path}")"
    mkdir -p "${log_dir}"
    {
      echo 'guest safe diagnostic'
      printf 'RUNNER_TOKEN=%s\n' "${MOCK_GUEST_TOKEN}"
      printf 'Authorization: token %s\n' "${MOCK_GUEST_AUTH_TOKEN}"
    } >"${log_dir}/mock.guest.log"
    {
      echo 'Docker-DinD safe diagnostic'
      printf 'EPAR_APP_PRIVATE_KEY=%s\n' "${MOCK_PRIVATE_KEY_ENV}"
    } >"${log_dir}/mock.docker-dind.log"
    echo 'pool safe diagnostic'
    printf 'RUNNER_TOKEN=%s\n' "${MOCK_POOL_TOKEN}"
    printf -- '--token %s\n' "${MOCK_CLI_TOKEN}"
    printf '{"token":"%s"}\n' "${MOCK_JSON_TOKEN}"
    printf 'Authorization: Bearer %s\n' "${MOCK_BEARER_TOKEN}"
    printf 'Authorization: Basic %s\n' "${MOCK_BASIC_AUTH}"
    printf '{"AcCeSsToKeN":"%s","client_secret":"%s","Password":"%s","private-key":"%s"}\n' \
      "${MOCK_JSON_ACCESS_TOKEN}" \
      "${MOCK_JSON_CLIENT_SECRET}" \
      "${MOCK_JSON_PASSWORD}" \
      "${MOCK_JSON_PRIVATE_KEY}"
    printf 'privateKey=%s\n' "${MOCK_PRIVATE_KEY_CAMEL}"
    printf 'GitHub tokens: %s %s %s %s %s %s\n' \
      "${MOCK_GHP_TOKEN}" \
      "${MOCK_GHO_TOKEN}" \
      "${MOCK_GHU_TOKEN}" \
      "${MOCK_GHS_TOKEN}" \
      "${MOCK_GHR_TOKEN}" \
      "${MOCK_GITHUB_PAT}"
    printf 'JWT: %s\n' "${MOCK_JWT}"
    echo '::warning::mock workflow-command injection'
    echo '-----BEGIN PRIVATE KEY-----'
    printf '%s\n' "${MOCK_KEY_BODY}"
    echo '-----END PRIVATE KEY-----'
    exit 7
  fi
  printf '%s\n' "$$" >"${MOCK_POOL_PID_FILE}"
  trap ': >"${MOCK_POOL_TERM_MARKER}"; exit 0' TERM INT
  while true; do sleep 1; done
fi
EOF
chmod +x "${test_root}/bin/"*

summary="${test_root}/summary.md"
EPAR_BINARY="${test_root}/bin/epar" \
EPAR_PROJECT_ROOT="${repo_root}" \
EPAR_APP_ID=12345 \
EPAR_ORGANIZATION=solutionforest \
EPAR_APP_PRIVATE_KEY='test-private-key-material' \
EPAR_TRUSTED_CA_CERTIFICATE_PATH="${test_root}/inspection-ca.pem" \
EPAR_DOCKER_REGISTRY_MIRROR=http://hubproxy.docker.internal:5555 \
EPAR_DOCKER_PROXY=http://host.docker.internal:3128 \
CORE_CANARY_LABEL=epar-core-123-1 \
CORE_POLL_SECONDS=1 \
GITHUB_TOKEN=test-workflow-token \
GITHUB_REPOSITORY=solutionforest/ephemeral-action-runner \
GITHUB_RUN_ID=123 \
GITHUB_RUN_ATTEMPT=1 \
GITHUB_API_URL=https://api.github.test \
GITHUB_STEP_SUMMARY="${summary}" \
RUNNER_TEMP="${test_root}/runner-temp" \
MOCK_EPAR_LOG="${test_root}/epar.log" \
MOCK_CA_PATH="${test_root}/inspection-ca.pem" \
MOCK_REGISTRY_MIRROR=http://hubproxy.docker.internal:5555 \
MOCK_DOCKER_PROXY=http://host.docker.internal:3128 \
MOCK_CANCEL_MARKER="${test_root}/cancelled" \
MOCK_POOL_PID_FILE="${test_root}/pool.pid" \
MOCK_POOL_TERM_MARKER="${test_root}/pool-term" \
MOCK_DOCKER_PS_COUNT="${test_root}/docker-ps-count" \
MOCK_DOCKER_LATE_RESOURCE_SCANS=1 \
MOCK_DOCKER_OUTSIDE_BOUNDARY_SCANS=1 \
MOCK_CLEANUP_CALL_COUNT_FILE="${test_root}/cleanup-count" \
MOCK_CLEANUP_FAIL_ON_CALL=2 \
MOCK_CLEANUP_FAIL_MARKER="${test_root}/cleanup-failed-once" \
CORE_CLEANUP_TOTAL_SECONDS=10 \
CORE_CLEANUP_ATTEMPT_SECONDS=1 \
CORE_CLEANUP_SETTLE_SECONDS=0 \
PATH="${test_root}/bin:${PATH}" \
bash "${repo_root}/scripts/ci/core-runner-controller.sh"

grep -q '^cleanup ' "${test_root}/epar.log"
grep -q '^image build --replace ' "${test_root}/epar.log"
grep -q '^pool up --instances 1 --monitor-interval 5s ' "${test_root}/epar.log"
grep -q 'Canary 1 runner.*epar-ci-core-20260710-100000-001' "${summary}"
grep -q 'Canary 2 runner.*epar-ci-core-20260710-100010-002' "${summary}"
test -f "${test_root}/pool-term"
test -f "${test_root}/cleanup-failed-once"
pool_pid="$(<"${test_root}/pool.pid")"
if kill -0 "${pool_pid}" 2>/dev/null; then
  echo "controller test found the pool supervisor still running after cleanup" >&2
  exit 1
fi
test "$(<"${test_root}/docker-ps-count")" -eq 3
test "$(<"${test_root}/cleanup-count")" -eq 5
test "$(grep -c '^cleanup ' "${test_root}/epar.log")" -eq 5
if grep -R -q 'test-private-key-material\|test-workflow-token' "${test_root}"; then
  echo "controller test found credential material after cleanup" >&2
  exit 1
fi
if find "${test_root}/runner-temp" -mindepth 1 -print -quit | grep -q .; then
  echo "controller test found run files after cleanup" >&2
  exit 1
fi

failure_output="${test_root}/failure-output.log"
rm -f "${test_root}/cancelled"
if EPAR_BINARY="${test_root}/bin/epar" \
  EPAR_PROJECT_ROOT="${repo_root}" \
  EPAR_APP_ID=12345 \
  EPAR_ORGANIZATION=solutionforest \
  EPAR_APP_PRIVATE_KEY='test-private-key-material' \
  EPAR_TRUSTED_CA_CERTIFICATE_PATH="${test_root}/inspection-ca.pem" \
  EPAR_DOCKER_REGISTRY_MIRROR=http://hubproxy.docker.internal:5555 \
  EPAR_DOCKER_PROXY=http://host.docker.internal:3128 \
  CORE_CANARY_LABEL=epar-core-123-1 \
  CORE_POLL_SECONDS=1 \
  GITHUB_TOKEN=test-workflow-token \
  GITHUB_REPOSITORY=solutionforest/ephemeral-action-runner \
  GITHUB_RUN_ID=124 \
  GITHUB_RUN_ATTEMPT=1 \
  GITHUB_API_URL=https://api.github.test \
  GITHUB_STEP_SUMMARY="${test_root}/failure-summary.md" \
  RUNNER_TEMP="${test_root}/runner-temp" \
  MOCK_EPAR_LOG="${test_root}/epar.log" \
  MOCK_CA_PATH="${test_root}/inspection-ca.pem" \
  MOCK_REGISTRY_MIRROR=http://hubproxy.docker.internal:5555 \
  MOCK_DOCKER_PROXY=http://host.docker.internal:3128 \
  MOCK_CANCEL_MARKER="${test_root}/cancelled" \
  MOCK_POOL_PID_FILE="${test_root}/failure-pool.pid" \
  MOCK_POOL_TERM_MARKER="${test_root}/failure-pool-term" \
  MOCK_DOCKER_PS_COUNT="${test_root}/failure-docker-ps-count" \
  CORE_CLEANUP_TOTAL_SECONDS=10 \
  CORE_CLEANUP_ATTEMPT_SECONDS=1 \
  CORE_CLEANUP_SETTLE_SECONDS=0 \
  MOCK_POOL_FAILURE=1 \
  MOCK_POOL_TOKEN=pool-secret-value \
  MOCK_CLI_TOKEN=cli-secret-value \
  MOCK_JSON_TOKEN=json-secret-value \
  MOCK_BEARER_TOKEN=bearer-secret-value \
  MOCK_BASIC_AUTH=basic-auth-secret-value \
  MOCK_JSON_ACCESS_TOKEN=json-access-token-secret-value \
  MOCK_JSON_CLIENT_SECRET=json-client-secret-value \
  MOCK_JSON_PASSWORD='json-password-before-\"after-secret-value' \
  MOCK_JSON_PRIVATE_KEY=json-private-key-secret-value \
  MOCK_PRIVATE_KEY_CAMEL=camel-private-key-secret-value \
  MOCK_GHP_TOKEN=ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA \
  MOCK_GHO_TOKEN=gho_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB \
  MOCK_GHU_TOKEN=ghu_CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC \
  MOCK_GHS_TOKEN=ghs_DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD \
  MOCK_GHR_TOKEN=ghr_EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE \
  MOCK_GITHUB_PAT=github_pat_FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF \
  MOCK_JWT=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJlcGFyLXRlc3QifQ.c2lnbmF0dXJlLXNlY3JldA \
  MOCK_GUEST_TOKEN=guest-secret-value \
  MOCK_GUEST_AUTH_TOKEN=guest-auth-secret-value \
  MOCK_PRIVATE_KEY_ENV=private-key-env-secret-value \
  MOCK_KEY_BODY=VGhpcy1pcy1mYWtlLXByaXZhdGUta2V5LW1hdGVyaWFs \
  PATH="${test_root}/bin:${PATH}" \
  bash "${repo_root}/scripts/ci/core-runner-controller.sh" >"${failure_output}" 2>&1; then
  echo "controller test expected the pool supervisor failure to fail" >&2
  exit 1
fi

grep -q 'EPAR pool supervisor exited unexpectedly with status 7' "${failure_output}"
grep -q 'EPAR pool supervisor log (last 200 lines, sanitized)' "${failure_output}"
grep -q 'EPAR runner log: mock.guest.log (last 200 lines, sanitized)' "${failure_output}"
grep -q 'EPAR runner log: mock.docker-dind.log (last 200 lines, sanitized)' "${failure_output}"
grep -q '| pool safe diagnostic' "${failure_output}"
grep -q '| guest safe diagnostic' "${failure_output}"
grep -q '| Docker-DinD safe diagnostic' "${failure_output}"
grep -q '| RUNNER_TOKEN=\*\*\*' "${failure_output}"
grep -q '| Authorization: Bearer \*\*\*' "${failure_output}"
grep -q '| Authorization: Basic \*\*\*' "${failure_output}"
grep -q '| {"token":"\*\*\*"}' "${failure_output}"
grep -q '| {"AcCeSsToKeN":"\*\*\*","client_secret":"\*\*\*","Password":"\*\*\*","private-key":"\*\*\*"}' "${failure_output}"
grep -q '| privateKey=\*\*\*' "${failure_output}"
grep -q '| GitHub tokens: \*\*\* \*\*\* \*\*\* \*\*\* \*\*\* \*\*\*' "${failure_output}"
grep -q '| JWT: \*\*\*' "${failure_output}"
grep -q '| \[REDACTED PRIVATE KEY\]' "${failure_output}"
grep -q '| ::warning::mock workflow-command injection' "${failure_output}"
if grep -q '^::warning::mock workflow-command injection' "${failure_output}"; then
  echo "controller test found an unescaped workflow command in diagnostics" >&2
  exit 1
fi
if grep -E -q 'pool-secret-value|cli-secret-value|json-secret-value|bearer-secret-value|basic-auth-secret-value|json-access-token-secret-value|json-client-secret-value|json-password-before|after-secret-value|json-private-key-secret-value|camel-private-key-secret-value|ghp_A+|gho_B+|ghu_C+|ghs_D+|ghr_E+|github_pat_F+|eyJhbGciOiJIUzI1NiJ9|guest-secret-value|guest-auth-secret-value|private-key-env-secret-value|VGhpcy1pcy1mYWtlLXByaXZhdGUta2V5LW1hdGVyaWFs' "${failure_output}"; then
  echo "controller test found secret material in failure diagnostics" >&2
  exit 1
fi
test -f "${test_root}/cancelled"
if find "${test_root}/runner-temp" -mindepth 1 -print -quit | grep -q .; then
  echo "controller test found run files after failed-run cleanup" >&2
  exit 1
fi

if EPAR_BINARY="${test_root}/bin/epar" \
  EPAR_PROJECT_ROOT="${repo_root}" \
  EPAR_APP_ID=not-a-number \
  EPAR_ORGANIZATION=solutionforest \
  EPAR_APP_PRIVATE_KEY='test-private-key-material' \
  CORE_CANARY_LABEL=epar-core-123-1 \
  GITHUB_TOKEN=test-workflow-token \
  GITHUB_REPOSITORY=solutionforest/ephemeral-action-runner \
  GITHUB_RUN_ID=123 \
  GITHUB_RUN_ATTEMPT=1 \
  GITHUB_API_URL=https://api.github.test \
  RUNNER_TEMP="${test_root}/runner-temp" \
  MOCK_CANCEL_MARKER="${test_root}/cancelled" \
  PATH="${test_root}/bin:${PATH}" \
  bash "${repo_root}/scripts/ci/core-runner-controller.sh"; then
  echo "controller test expected invalid initialization to fail" >&2
  exit 1
fi
test -f "${test_root}/cancelled"
rm -f "${test_root}/cancelled"

if EPAR_BINARY="${test_root}/bin/epar" \
  EPAR_PROJECT_ROOT="${repo_root}" \
  EPAR_APP_ID=12345 \
  EPAR_ORGANIZATION=solutionforest \
  EPAR_APP_PRIVATE_KEY='test-private-key-material' \
  EPAR_DOCKER_REGISTRY_MIRROR=file:///unsafe-registry \
  CORE_CANARY_LABEL=epar-core-123-1 \
  GITHUB_TOKEN=test-workflow-token \
  GITHUB_REPOSITORY=solutionforest/ephemeral-action-runner \
  GITHUB_RUN_ID=123 \
  GITHUB_RUN_ATTEMPT=1 \
  GITHUB_API_URL=https://api.github.test \
  RUNNER_TEMP="${test_root}/runner-temp" \
  MOCK_CANCEL_MARKER="${test_root}/cancelled" \
  PATH="${test_root}/bin:${PATH}" \
  bash "${repo_root}/scripts/ci/core-runner-controller.sh"; then
  echo "controller test expected an invalid registry mirror URL to fail" >&2
  exit 1
fi
test -f "${test_root}/cancelled"

rm -f "${test_root}/cancelled"
if EPAR_BINARY="${test_root}/bin/epar" \
  EPAR_PROJECT_ROOT="${repo_root}" \
  EPAR_APP_ID=12345 \
  EPAR_ORGANIZATION=solutionforest \
  EPAR_APP_PRIVATE_KEY='test-private-key-material' \
  EPAR_DOCKER_PROXY=http://user:password@proxy.internal:3128 \
  CORE_CANARY_LABEL=epar-core-123-1 \
  GITHUB_TOKEN=test-workflow-token \
  GITHUB_REPOSITORY=solutionforest/ephemeral-action-runner \
  GITHUB_RUN_ID=123 \
  GITHUB_RUN_ATTEMPT=1 \
  GITHUB_API_URL=https://api.github.test \
  RUNNER_TEMP="${test_root}/runner-temp" \
  MOCK_CANCEL_MARKER="${test_root}/cancelled" \
  PATH="${test_root}/bin:${PATH}" \
  bash "${repo_root}/scripts/ci/core-runner-controller.sh"; then
  echo "controller test expected proxy userinfo to fail" >&2
  exit 1
fi
test -f "${test_root}/cancelled"

echo "core runner controller test passed"
