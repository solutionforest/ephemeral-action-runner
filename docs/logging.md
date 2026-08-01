# Logging

EPAR writes logs under `work/logs` by default. Use `./start logs path` to print the resolved directory.

```text
work/logs/
├── epar.log
├── epar-last-error.log
├── errors/
├── instances/
├── builds/
└── benchmarks/
```

## What Goes Where

Manager events describe EPAR decisions and progress. Command transcripts contain raw instance, provider, and image-build output.

The local default sends manager events to the console and transcripts to files:

```yaml
logging:
  managerSinks: [console]
  managerConsoleFormat: text
  transcriptSinks: [file]
```

Manager file events use `epar.log`. Instance transcripts use `instances/`, build and source transcripts use `builds/`, startup timing records use `benchmarks/`, and timestamped error reports use `errors/`. `epar-last-error.log` always points to the latest error report and is never removed by retention.

Runner logs inside an Ubuntu guest are normally:

- `/var/log/actions-runner/run.log`
- `/opt/actions-runner/_diag`
- `/var/log/epar-dockerd.log` when the runner has a private Docker daemon

When runner launch or GitHub readiness fails, EPAR appends bounded process and runner diagnostics to the matching host-side instance transcript.

## Console And File Formats

Console and file sinks can use `text` or `json`. For Kubernetes or another runtime that already collects standard output, send both event types to the console:

```yaml
logging:
  managerSinks: [console]
  managerConsoleFormat: json
  transcriptSinks: [console]
  transcriptConsoleFormat: json
```

Text templates can change the human-readable console layout. Manager templates support `{time}`, `{level}`, `{message}`, and `{attributes}`. Transcript templates also support instance, component, stream, session, category, and provider fields. A template must contain `{message}` and is invalid when the corresponding console format is JSON.

See [Configuration](configuration.md#logging) for every logging property, default, and validation rule.

## Rotation And Retention

EPAR rotates active manager and transcript files at `logging.maxFileSizeMiB`, retains the configured number of compressed backups, applies category age limits, then enforces the total retained-size budget. It protects active files across EPAR processes, does not follow links or reparse points, and ignores unknown files.

Inspect or preview recognized log maintenance with:

```bash
./start logs list
./start logs prune --dry-run
```

Remove `--dry-run` only after reviewing the exact retention plan.

Wrapper control files and command results are state, not logs. Current wrappers place them under `work/state`, outside log retention.

## Shipping Logs

EPAR does not embed vendor-specific or OTLP clients. Use an external collector when logs must leave the host. The [`examples/observability`](../examples/observability/README.md) directory includes local file, Kubernetes console, and OpenTelemetry Collector examples.
