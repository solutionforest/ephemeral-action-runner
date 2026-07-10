#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf "${test_root}"' EXIT
mkdir -p "${test_root}/bin" "${test_root}/runner-temp"
printf '%s\n' 'test inspection CA' >"${test_root}/inspection-ca.pem"

workflow_path="${repo_root}/.github/workflows/core-runner-verification.yml"
grep -F -q 'if: ${{ failure() && !cancelled() }}' "${workflow_path}"
grep -F -q 'Cancel workflow after controller setup failure' "${workflow_path}"
grep -F -q '"${api_url}/force-cancel"' "${workflow_path}"

cat >"${test_root}/bin/docker" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat >"${test_root}/bin/curl" <<'EOF'
#!/usr/bin/env bash
if [[ " $* " == *" --request GET "* ]]; then
  cat <<'JSON'
{"jobs":[
  {"name":"Core canary 1","status":"completed","conclusion":"success","runner_name":"epar-ci-core-20260710-100000-001","runner_group_name":"epar-ci-canary","labels":["epar-core-123-1"]},
  {"name":"Core canary 2","status":"completed","conclusion":"success","runner_name":"epar-ci-core-20260710-100010-002","runner_group_name":"epar-ci-canary","labels":["epar-core-123-1"]}
]}
JSON
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
    printf '%s\n' '{"name":"Core canary 1","status":"completed","conclusion":"success","runner_name":"epar-ci-core-20260710-100000-001","runner_group_name":"epar-ci-canary","labels":["epar-core-123-1"]}'
    ;;
  *'Core canary 2'*)
    printf '%s\n' '{"name":"Core canary 2","status":"completed","conclusion":"success","runner_name":"epar-ci-core-20260710-100010-002","runner_group_name":"epar-ci-canary","labels":["epar-core-123-1"]}'
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
  trap 'exit 0' TERM INT
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
PATH="${test_root}/bin:${PATH}" \
bash "${repo_root}/scripts/ci/core-runner-controller.sh"

grep -q '^cleanup ' "${test_root}/epar.log"
grep -q '^image build --replace ' "${test_root}/epar.log"
grep -q '^pool up --instances 1 --monitor-interval 5s ' "${test_root}/epar.log"
grep -q 'Canary 1 runner.*epar-ci-core-20260710-100000-001' "${summary}"
grep -q 'Canary 2 runner.*epar-ci-core-20260710-100010-002' "${summary}"
if grep -R -q 'test-private-key-material\|test-workflow-token' "${test_root}"; then
  echo "controller test found credential material after cleanup" >&2
  exit 1
fi
if find "${test_root}/runner-temp" -mindepth 1 -print -quit | grep -q .; then
  echo "controller test found run files after cleanup" >&2
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
