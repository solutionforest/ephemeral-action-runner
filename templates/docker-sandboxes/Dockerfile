# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG TEMPLATE_PLATFORM=linux/amd64
ARG SOURCE_IMAGE=ghcr.io/catthehacker/ubuntu@sha256:b40b8af93baee90b83f29c834440873300c8478809535786dbf79fa836c086ac
ARG GO_BUILDER_IMAGE=docker.io/library/golang@sha256:9006890ecba0a168034d99516084099ae3114d9f2b7d6572c77f2dde57ebc980

FROM --platform=$BUILDPLATFORM ${GO_BUILDER_IMAGE} AS hook-builder
ARG TARGETOS
ARG TARGETARCH
ARG HOOK_LAUNCHER_SHA256=7fe07f10f484fa6888481a4165e81570187c0aeff422738d3ea5add6b95dd9b7
WORKDIR /src
COPY hook-launcher/main.go /src/main.go
RUN echo "${HOOK_LAUNCHER_SHA256}  /src/main.go" | sha256sum --check - \
    && install -d -m 0755 /out \
    && CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" GOTOOLCHAIN=local go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o /out/epar-hook-bash /src/main.go

FROM --platform=${TEMPLATE_PLATFORM} ${SOURCE_IMAGE}

ARG TEMPLATE_PLATFORM
ARG SOURCE_IMAGE
ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG SOURCE_PROFILE=act-22.04
ARG SOURCE_INDEX_DIGEST=sha256:b40b8af93baee90b83f29c834440873300c8478809535786dbf79fa836c086ac
ARG SOURCE_MANIFEST_DIGEST=sha256:f3d493b10df1582ce631e0213bd90aa5f8196287c8a9f8ef546ecb44ca256655
ARG SOURCE_REVISION=e2f8efe464c82732f78e967ee709c00b6af53643
ARG TEMPLATE_VERSION=20260723-r4-amd64
ARG COMPATIBILITY_FILE=act-22.04.amd64.compatibility.json
ARG ACTIONS_RUNNER_URL=https://github.com/actions/runner/releases/download/v2.332.0/actions-runner-linux-x64-2.332.0.tar.gz
ARG ACTIONS_RUNNER_SHA256=sha256:f2094522a6b9afeab07ffb586d1eb3f190b6457074282796c497ce7dce9e0f2a
ARG TINI_URL=https://github.com/krallin/tini/releases/download/v0.19.0/tini-amd64
ARG TINI_SHA256=sha256:93dcc18adc78c65a028a84799ecf8ad40c936fdfc5f2a57b1acda5a8117fa82c

USER root
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

COPY guest/ /opt/epar/
COPY helpers.sha256 /opt/epar/helpers.sha256
COPY profiles/${COMPATIBILITY_FILE} /opt/epar/template-compatibility.json
COPY --from=hook-builder --chmod=0555 /out/epar-hook-bash /opt/epar/hook-bin/bash

ADD --chmod=0755 --checksum=${TINI_SHA256} ${TINI_URL} /usr/local/bin/tini
ADD --checksum=${ACTIONS_RUNNER_SHA256} ${ACTIONS_RUNNER_URL} /tmp/actions-runner.tar.gz

RUN [[ "${TARGETPLATFORM}" == "${TEMPLATE_PLATFORM}" ]] \
    && [[ "${TARGETOS}" == "linux" ]] \
    && [[ "${TARGETARCH}" == "amd64" || "${TARGETARCH}" == "arm64" ]] \
    && cd /opt/epar \
    && sha256sum --check helpers.sha256 \
    && chmod 0555 ./*.sh \
    && /opt/epar/prepare-template.sh \
    && rm -rf /opt/actions-runner \
    && install -d -m 0755 -o agent -g agent /opt/actions-runner /var/log/actions-runner \
    && tar -xzf /tmp/actions-runner.tar.gz -C /opt/actions-runner \
    && rm -f /tmp/actions-runner.tar.gz \
    && chown -R agent:agent /opt/actions-runner /var/log/actions-runner \
    && sudo -u agent -H /opt/actions-runner/bin/Runner.Listener --version | grep -Fx '2.332.0' \
    && /usr/local/bin/tini --version 2>&1 | grep -F 'version 0.19.0'

ENV HOME=/home/agent \
    USER=agent \
    LOGNAME=agent \
    EPAR_ACTIONS_RUNNER_DIR=/opt/actions-runner \
    EPAR_RUNNER_WORK_DIR=/opt/actions-runner \
    EPAR_TEMPLATE_SBX_VERSION=0.35.0 \
    EPAR_TEMPLATE_PLATFORM=${TEMPLATE_PLATFORM} \
    DEBIAN_FRONTEND=dialog

LABEL com.docker.sandboxes.start-docker=true \
    io.solutionforest.epar.template.candidate="A" \
    io.solutionforest.epar.template.profile="${SOURCE_PROFILE}" \
    io.solutionforest.epar.template.platform="${TEMPLATE_PLATFORM}" \
    io.solutionforest.epar.template.sbx-version="0.35.0" \
    io.solutionforest.epar.template.runner-version="2.332.0" \
    io.solutionforest.epar.template.source-index-digest="${SOURCE_INDEX_DIGEST}" \
    io.solutionforest.epar.template.source-manifest-digest="${SOURCE_MANIFEST_DIGEST}" \
    org.opencontainers.image.base.name="${SOURCE_IMAGE}" \
    org.opencontainers.image.revision="${SOURCE_REVISION}" \
    org.opencontainers.image.version="${TEMPLATE_VERSION}"

WORKDIR /home/agent
USER agent
ENTRYPOINT ["/usr/local/bin/tini", "-g", "--", "/opt/epar/template-entrypoint.sh"]
CMD ["sleep", "infinity"]
