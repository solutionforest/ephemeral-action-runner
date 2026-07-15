# Observability Examples

`local-file.yml` shows the normal workstation configuration with manager and transcript files enabled. `kubernetes-console.yml` shows cloud-native JSON console logging. These fragments contain the complete strict `logging` section and can replace that section in an EPAR config.

`otel-collector-filelog.yaml` shows an OpenTelemetry Collector Contrib `filelog` receiver for an EPAR log volume. Mount the same log directory into the Collector at `/var/log/epar`, configure a production exporter, and persist the Collector storage directory. The example does not delete source files; EPAR remains responsible for retention.
