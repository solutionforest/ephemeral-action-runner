# Go toolchain + Docker CLI, for running EPAR from source with no local Go
# install (scripts/run-with-docker.sh). The Docker CLI lets EPAR's own
# runtime docker calls reach the host daemon over the mounted socket.
ARG GO_IMAGE=golang:1.24
FROM ${GO_IMAGE}
COPY --from=docker:27-cli /usr/local/bin/docker /usr/local/bin/docker
