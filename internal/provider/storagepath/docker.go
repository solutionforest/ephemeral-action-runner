package storagepath

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

type DockerEndpointClass string

const (
	DockerEndpointLocal  DockerEndpointClass = "local"
	DockerEndpointRemote DockerEndpointClass = "remote"
)

var (
	ErrRemoteDockerEndpoint       = errors.New("remote Docker endpoint")
	ErrUnsupportedDockerTransport = errors.New("unsupported Docker transport")
	ErrInvalidDockerStorage       = errors.New("invalid Docker storage observation")
	ErrDockerCapacityUnavailable  = errors.New("Docker capacity path is unavailable")
)

type DockerInfo struct {
	DockerRootDir   string     `json:"DockerRootDir"`
	OperatingSystem string     `json:"OperatingSystem"`
	Name            string     `json:"Name"`
	DriverStatus    [][]string `json:"DriverStatus"`
}

type DockerStorage struct {
	Endpoint      string
	EndpointClass DockerEndpointClass
	Desktop       bool
	Roots         []Resolution
}

type DockerDiscoveryOptions struct {
	Environment  Environment
	Endpoint     string
	Info         DockerInfo
	ReadFile     func(string) ([]byte, error)
	Stat         func(string) (os.FileInfo, error)
	CapacityPath func(string) (string, error)
}

func ClassifyDockerEndpoint(endpoint string) (DockerEndpointClass, error) {
	endpoint = strings.TrimSpace(strings.Trim(endpoint, "\""))
	if endpoint == "" {
		return "", fmt.Errorf("%w: Docker endpoint is unavailable", ErrInvalidDockerStorage)
	}
	lower := strings.ToLower(endpoint)
	for _, prefix := range []string{"unix://", "npipe://", "fd://"} {
		if strings.HasPrefix(lower, prefix) {
			return DockerEndpointLocal, nil
		}
	}
	for _, prefix := range []string{"ssh://", "tcp://", "http://", "https://"} {
		if strings.HasPrefix(lower, prefix) {
			return DockerEndpointRemote, nil
		}
	}
	return "", fmt.Errorf("%w: Docker endpoint %q", ErrUnsupportedDockerTransport, endpoint)
}

// DiscoverDockerStorage resolves the host-visible backing store for one Docker
// context. Remote contexts are rejected because a local filesystem probe cannot
// establish capacity for a remote daemon.
func DiscoverDockerStorage(options DockerDiscoveryOptions) (DockerStorage, error) {
	class, err := ClassifyDockerEndpoint(options.Endpoint)
	if err != nil {
		return DockerStorage{}, err
	}
	if class == DockerEndpointRemote {
		return DockerStorage{}, fmt.Errorf("%w %q has no host-local authoritative capacity path", ErrRemoteDockerEndpoint, options.Endpoint)
	}
	result := DockerStorage{Endpoint: options.Endpoint, EndpointClass: class, Desktop: isDockerDesktop(options.Info)}
	if result.Desktop {
		root, discoverErr := discoverDesktopRoot(options)
		if discoverErr != nil {
			if errors.Is(discoverErr, ErrDockerCapacityUnavailable) {
				result.Roots = []Resolution{unavailableResolution("engine", root.Path, root.Provenance, discoverErr)}
				return result, nil
			}
			return DockerStorage{}, fmt.Errorf("%w: %v", ErrInvalidDockerStorage, discoverErr)
		}
		result.Roots = []Resolution{root}
		return result, nil
	}
	if !isAbs(options.Environment.GOOS, options.Info.DockerRootDir) {
		return DockerStorage{}, fmt.Errorf("%w: Docker info root %q is not an absolute host path", ErrInvalidDockerStorage, options.Info.DockerRootDir)
	} else {
		resolved, resolveErr := resolveObservedDirectory(options, options.Info.DockerRootDir)
		if resolveErr != nil {
			result.Roots = append(result.Roots, unavailableResolution("engine", clean(options.Environment.GOOS, options.Info.DockerRootDir), ProvenanceDockerInfo, fmt.Errorf("resolve Docker Engine capacity path: %w", resolveErr)))
		} else {
			result.Roots = append(result.Roots, Resolution{ID: "engine", Path: clean(options.Environment.GOOS, options.Info.DockerRootDir), CapacityPath: resolved, Provenance: ProvenanceDockerInfo, Confidence: ConfidenceObserved})
		}
	}
	if containerdImageStoreActive(options.Info) {
		containerdRoot, rootErr := nativeContainerdRoot(options)
		if rootErr != nil {
			if errors.Is(rootErr, ErrInvalidDockerStorage) {
				return DockerStorage{}, rootErr
			}
			path := containerdRoot.Path
			if path == "" {
				path = "/var/lib/containerd"
			}
			provenance := containerdRoot.Provenance
			if provenance == "" {
				provenance = ProvenancePlatformDefault
			}
			containerdRoot = unavailableResolution("containerd", path, provenance, rootErr)
		}
		result.Roots = append(result.Roots, containerdRoot)
	}
	return result, nil
}

func DiscoverCurrentDockerStorage(ctx context.Context) (DockerStorage, error) {
	environment, err := CurrentEnvironment()
	if err != nil {
		return DockerStorage{}, fmt.Errorf("%w: discover local storage environment: %v", ErrInvalidDockerStorage, err)
	}
	endpoint := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if endpoint == "" {
		endpointBytes, err := runDocker(ctx, "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}")
		if err != nil {
			return DockerStorage{}, fmt.Errorf("inspect Docker context endpoint: %w", err)
		}
		if err := json.Unmarshal(bytes.TrimSpace(endpointBytes), &endpoint); err != nil {
			return DockerStorage{}, fmt.Errorf("%w: decode Docker context endpoint: %v", ErrInvalidDockerStorage, err)
		}
	}
	classification, err := ClassifyDockerEndpoint(endpoint)
	if err != nil {
		return DockerStorage{}, err
	}
	if classification == DockerEndpointRemote {
		return DockerStorage{}, fmt.Errorf("%w %q has no host-local authoritative capacity path", ErrRemoteDockerEndpoint, endpoint)
	}
	infoBytes, err := runDocker(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return DockerStorage{}, fmt.Errorf("%w: inspect Docker Engine storage: %v", ErrDockerCapacityUnavailable, err)
	}
	var info DockerInfo
	if err := json.Unmarshal(infoBytes, &info); err != nil {
		return DockerStorage{}, fmt.Errorf("%w: decode Docker Engine storage: %v", ErrInvalidDockerStorage, err)
	}
	return DiscoverDockerStorage(DockerDiscoveryOptions{Environment: environment, Endpoint: endpoint, Info: info})
}

func unavailableResolution(id, path string, provenance Provenance, cause error) Resolution {
	reason := "capacity path is unavailable"
	if cause != nil {
		reason = cause.Error()
	}
	return Resolution{
		ID:                        id,
		Path:                      path,
		CapacityUnavailableReason: reason,
		Provenance:                provenance,
		Confidence:                ConfidenceUnavailable,
	}
}

func runDocker(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", arguments...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func isDockerDesktop(info DockerInfo) bool {
	identity := strings.ToLower(info.OperatingSystem + " " + info.Name)
	return strings.Contains(identity, "docker desktop") || strings.Contains(identity, "docker-desktop")
}

func containerdImageStoreActive(info DockerInfo) bool {
	for _, status := range info.DriverStatus {
		if len(status) < 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(status[0]))
		value := strings.ToLower(strings.TrimSpace(status[1]))
		if key == "driver-type" && strings.Contains(value, "containerd") {
			return true
		}
	}
	return false
}

func nativeContainerdRoot(options DockerDiscoveryOptions) (Resolution, error) {
	root := "/var/lib/containerd"
	provenance := ProvenancePlatformDefault
	candidate := func() Resolution {
		return Resolution{ID: "containerd", Path: root, Provenance: provenance}
	}
	if options.Environment.GOOS != "linux" {
		return candidate(), fmt.Errorf("active containerd image store root discovery is unsupported on native %s Docker Engine", options.Environment.GOOS)
	}
	readFile := options.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	data, err := readFile("/etc/containerd/config.toml")
	if err == nil {
		if configured := parseContainerdRoot(data); configured != "" {
			root = clean("linux", configured)
			provenance = ProvenanceContainerdConfig
			if !isAbs("linux", configured) {
				return candidate(), fmt.Errorf("%w: containerd root %q is not absolute", ErrInvalidDockerStorage, configured)
			}
		}
	} else if !os.IsNotExist(err) {
		return candidate(), fmt.Errorf("read containerd configuration: %w", err)
	}
	resolved, err := resolveObservedDirectory(options, root)
	if err != nil {
		return candidate(), fmt.Errorf("resolve containerd capacity path: %w", err)
	}
	return Resolution{ID: "containerd", Path: root, CapacityPath: resolved, Provenance: provenance, Confidence: ConfidenceDerived}, nil
}

func resolveObservedDirectory(options DockerDiscoveryOptions, root string) (string, error) {
	if options.CapacityPath != nil {
		return options.CapacityPath(root)
	}
	stat := options.Stat
	if stat == nil {
		stat = os.Stat
	}
	info, err := stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("observed storage root %q is not a directory", root)
	}
	return currentCapacityPath(root)
}

func parseContainerdRoot(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return ""
		}
		if !strings.HasPrefix(trimmed, "root") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) != "root" {
			continue
		}
		var value string
		if err := json.Unmarshal([]byte(strings.TrimSpace(parts[1])), &value); err == nil {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func discoverDesktopRoot(options DockerDiscoveryOptions) (Resolution, error) {
	settingsPath, defaultDisk, err := desktopPaths(options.Environment)
	if err != nil {
		return Resolution{}, err
	}
	readFile := options.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	stat := options.Stat
	if stat == nil {
		stat = os.Stat
	}
	candidates := make(map[string]string)
	settings, readErr := readFile(settingsPath)
	if readErr == nil {
		var document any
		if err := json.Unmarshal(settings, &document); err != nil {
			return Resolution{}, fmt.Errorf("decode Docker Desktop settings store %s: %w", settingsPath, err)
		}
		for _, value := range recursiveStrings(document) {
			if !isAbs(options.Environment.GOOS, value) {
				continue
			}
			info, statErr := stat(value)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					continue
				}
				return Resolution{ID: "engine", Path: clean(options.Environment.GOOS, value), Provenance: ProvenanceDocumentedSettings}, fmt.Errorf("%w: inspect Docker Desktop settings path %s: %v", ErrDockerCapacityUnavailable, value, statErr)
			}
			if !info.IsDir() {
				if isDesktopDisk(options.Environment.GOOS, value) {
					addDesktopDiskCandidate(candidates, options.Environment.GOOS, value)
				}
				continue
			}
			for _, candidate := range desktopDiskCandidates(options.Environment.GOOS, value) {
				candidateInfo, candidateErr := stat(candidate)
				if candidateErr != nil {
					if os.IsNotExist(candidateErr) {
						continue
					}
					return Resolution{ID: "engine", Path: clean(options.Environment.GOOS, candidate), Provenance: ProvenanceDocumentedSettings}, fmt.Errorf("%w: inspect Docker Desktop settings disk %s: %v", ErrDockerCapacityUnavailable, candidate, candidateErr)
				}
				if !candidateInfo.IsDir() {
					addDesktopDiskCandidate(candidates, options.Environment.GOOS, candidate)
				}
			}
		}
	} else if !os.IsNotExist(readErr) {
		return Resolution{ID: "engine", Path: defaultDisk, Provenance: ProvenanceDocumentedDefaultAssumed}, fmt.Errorf("%w: read Docker Desktop settings store %s: %v", ErrDockerCapacityUnavailable, settingsPath, readErr)
	}
	if len(candidates) > 1 {
		paths := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			paths = append(paths, candidate)
		}
		sort.Strings(paths)
		return Resolution{}, fmt.Errorf("Docker Desktop settings store contains multiple existing backing disks: %s", strings.Join(paths, ", "))
	}
	capacityPath := options.CapacityPath
	if capacityPath == nil {
		capacityPath = currentCapacityPath
	}
	for _, candidate := range candidates {
		resolved, err := capacityPath(candidate)
		if err != nil {
			return Resolution{ID: "engine", Path: candidate, Provenance: ProvenanceDocumentedSettings}, fmt.Errorf("%w: resolve Docker Desktop settings backing disk: %v", ErrDockerCapacityUnavailable, err)
		}
		return Resolution{ID: "engine", Path: candidate, CapacityPath: resolved, Provenance: ProvenanceDocumentedSettings, Confidence: ConfidenceObserved}, nil
	}
	if info, err := stat(defaultDisk); err == nil && !info.IsDir() {
		resolved, resolveErr := capacityPath(defaultDisk)
		if resolveErr != nil {
			return Resolution{ID: "engine", Path: defaultDisk, Provenance: ProvenanceDocumentedDefaultObserved}, fmt.Errorf("%w: resolve Docker Desktop default backing disk: %v", ErrDockerCapacityUnavailable, resolveErr)
		}
		return Resolution{ID: "engine", Path: defaultDisk, CapacityPath: resolved, Provenance: ProvenanceDocumentedDefaultObserved, Confidence: ConfidenceObserved}, nil
	} else if err != nil && !os.IsNotExist(err) {
		return Resolution{ID: "engine", Path: defaultDisk, Provenance: ProvenanceDocumentedDefaultAssumed}, fmt.Errorf("%w: inspect Docker Desktop default backing disk: %v", ErrDockerCapacityUnavailable, err)
	}
	resolved, err := capacityPath(defaultDisk)
	if err != nil {
		return Resolution{ID: "engine", Path: defaultDisk, Provenance: ProvenanceDocumentedDefaultAssumed}, fmt.Errorf("%w: resolve Docker Desktop assumed backing disk ancestor: %v", ErrDockerCapacityUnavailable, err)
	}
	warning := fmt.Sprintf("Docker Desktop backing disk was not observed; capacity is measured at nearest existing ancestor %s of documented default %s", resolved, defaultDisk)
	return Resolution{ID: "engine", Path: defaultDisk, CapacityPath: resolved, Provenance: ProvenanceDocumentedDefaultAssumed, Confidence: ConfidenceAssumed, Warnings: []string{warning}}, nil
}

func desktopDiskCandidates(goos, directory string) []string {
	if goos == "windows" {
		return []string{
			join(goos, directory, "docker_data.vhdx"),
			join(goos, directory, "disk", "docker_data.vhdx"),
			join(goos, directory, "data", "docker_data.vhdx"),
			join(goos, directory, "wsl", "disk", "docker_data.vhdx"),
			join(goos, directory, "wsl", "data", "docker_data.vhdx"),
		}
	}
	return []string{
		join(goos, directory, "Docker.raw"),
		join(goos, directory, "data", "Docker.raw"),
		join(goos, directory, "vms", "0", "data", "Docker.raw"),
	}
}

func addDesktopDiskCandidate(candidates map[string]string, goos, value string) {
	candidate := clean(goos, value)
	key := candidate
	if goos == "windows" {
		key = strings.ToLower(key)
	}
	candidates[key] = candidate
}

func desktopPaths(environment Environment) (string, string, error) {
	switch environment.GOOS {
	case "windows":
		if !isAbs("windows", environment.AppData) || !isAbs("windows", environment.LocalAppData) {
			return "", "", errors.New("APPDATA and LOCALAPPDATA must be absolute for Docker Desktop storage discovery")
		}
		return join("windows", environment.AppData, "Docker", "settings-store.json"), join("windows", environment.LocalAppData, "Docker", "wsl", "disk", "docker_data.vhdx"), nil
	case "darwin":
		if !isAbs("darwin", environment.HomeDir) {
			return "", "", errors.New("home directory must be absolute for Docker Desktop storage discovery")
		}
		return join("darwin", environment.HomeDir, "Library", "Group Containers", "group.com.docker", "settings-store.json"), join("darwin", environment.HomeDir, "Library", "Containers", "com.docker.docker", "Data", "vms", "0", "data", "Docker.raw"), nil
	case "linux":
		if !isAbs("linux", environment.HomeDir) {
			return "", "", errors.New("home directory must be absolute for Docker Desktop storage discovery")
		}
		return join("linux", environment.HomeDir, ".docker", "desktop", "settings-store.json"), join("linux", environment.HomeDir, ".docker", "desktop", "vms", "0", "data", "Docker.raw"), nil
	default:
		return "", "", fmt.Errorf("Docker Desktop storage discovery is unsupported on %s", environment.GOOS)
	}
}

func isDesktopDisk(goos, value string) bool {
	name := base(goos, value)
	return strings.EqualFold(name, "Docker.raw") || strings.EqualFold(name, "docker_data.vhdx")
}

func recursiveStrings(value any) []string {
	var result []string
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case string:
			result = append(result, typed)
		case []any:
			for _, item := range typed {
				visit(item)
			}
		case map[string]any:
			for _, item := range typed {
				visit(item)
			}
		}
	}
	visit(value)
	return result
}
