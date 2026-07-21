# Logging

EPAR uses `work/logs` under the project root by default. Set `logging.directory` to an absolute path to store logs elsewhere, or to another relative path to resolve it from the project root.

```text
work/logs/
├── epar.log                 # when managerSinks includes file
├── epar-last-error.log
├── errors/
├── instances/
├── builds/
└── benchmarks/
```

Manager events and command transcripts have independent sinks. `console` and `file` may be selected separately or together. Console manager events route debug and info to stdout and warnings and errors to stderr. File manager events go to `epar.log`. File transcripts remain raw command output; console transcripts frame every completed stdout or stderr line with timestamp, instance, component, and stream context. JSON console mode emits one JSON object per line.

The local default keeps manager events on the console and command transcripts in files:

```yaml
logging:
  directory: work/logs
  managerSinks: [console]
  managerConsoleFormat: text
  managerConsoleTextFormat: "{time} [{level}] {message}"
  managerFileFormat: json
  transcriptSinks: [file]
  transcriptConsoleFormat: text
  maxFileSizeMiB: 100
  maxBackups: 3
  compressBackups: true
  retentionEnabled: true
  retentionMaxTotalMiB: 1024
  managerMaxAgeDays: 14
  instanceMaxAgeDays: 14
  buildMaxAgeDays: 14
  errorMaxAgeDays: 30
  benchmarkMaxAgeDays: 90
  retentionIntervalMinutes: 60
```

The default manager console line is compact and human-readable:

```text
2026-07-16T00:14:58.108+08:00 [INFO] cloning instance
```

When `managerConsoleFormat` is `text`, `managerConsoleTextFormat` accepts `{time}`, `{level}`, `{message}`, and `{attributes}`. `{message}` is required. The default deliberately omits structured attributes for concise local output. To include them, use `"{time} [{level}] {message}{attributes}"`; `{attributes}` expands to structured fields with its own leading space when fields exist.

When `transcriptConsoleFormat` is `text`, the optional `transcriptConsoleTextFormat` accepts `{time}`, `{instance}`, `{component}`, `{stream}`, `{message}`, `{session}`, `{category}`, `{provider}`, and `{attributes}`. Its default is `"{time} {stream} {instance} {component} {message}{attributes}"`.

Custom text formats are rejected when the corresponding console format is `json`. JSON records keep their fixed structured schema so downstream parsers and log shippers can rely on it.

For Kubernetes, use console sinks so the container runtime can collect stdout and stderr:

```yaml
logging:
  directory: work/logs
  managerSinks: [console]
  managerConsoleFormat: json
  managerFileFormat: json
  transcriptSinks: [console]
  transcriptConsoleFormat: json
  maxFileSizeMiB: 100
  maxBackups: 3
  compressBackups: true
  retentionEnabled: true
  retentionMaxTotalMiB: 1024
  managerMaxAgeDays: 14
  instanceMaxAgeDays: 14
  buildMaxAgeDays: 14
  errorMaxAgeDays: 30
  benchmarkMaxAgeDays: 90
  retentionIntervalMinutes: 60
```

Benchmark JSONL and error reports remain file artifacts in every sink mode. EPAR rotates active manager and transcript files at the configured size, retains the configured number of gzip-compressed backups, and applies category age limits before the aggregate size budget. It protects active files across EPAR processes, does not follow links or reparse points, ignores unknown files, and never removes `epar-last-error.log`.

Wrapper control files and command result files are state rather than logs. New wrappers place them under `work/state`, outside retention scope.

Use `ephemeral-action-runner logs path` to find the resolved root, `ephemeral-action-runner logs list` to inspect recognized artifacts, and `ephemeral-action-runner logs prune --dry-run` to preview retention. Remove `--dry-run` to prune immediately.

EPAR intentionally does not embed OTLP or vendor-specific clients. To ship file artifacts, use an external agent such as the OpenTelemetry Collector `filelog` receiver. See [`examples/observability`](../examples/observability/README.md).
