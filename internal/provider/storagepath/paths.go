// Package storagepath discovers host-visible provider storage locations.
//
// Discovery is deliberately read-only. A returned path is capacity evidence;
// it never grants ownership or cleanup authority over the discovered object.
package storagepath

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

type Confidence string

const (
	ConfidenceObserved    Confidence = "observed"
	ConfidenceDerived     Confidence = "derived"
	ConfidenceAssumed     Confidence = "assumed"
	ConfidenceUnavailable Confidence = "unavailable"
)

type Provenance string

const (
	ProvenanceDockerInfo                Provenance = "docker-info"
	ProvenanceContainerdConfig          Provenance = "containerd-config"
	ProvenanceDocumentedSettings        Provenance = "documented-settings-store"
	ProvenanceDocumentedDefaultObserved Provenance = "documented-default-observed"
	ProvenanceDocumentedDefaultAssumed  Provenance = "documented-default-assumed"
	ProvenanceEnvironment               Provenance = "environment"
	ProvenancePlatformDefault           Provenance = "platform-default"
)

// Resolution separates the provider locator from the existing directory used
// for capacity measurement. CapacityPath may be a redirected canonical target.
// Neither field is cleanup ownership evidence.
type Resolution struct {
	ID                        string
	Path                      string
	CapacityPath              string
	CapacityUnavailableReason string
	Provenance                Provenance
	Confidence                Confidence
	Warnings                  []string
}

type Environment struct {
	GOOS          string
	HomeDir       string
	LocalAppData  string
	AppData       string
	XDGStateHome  string
	XDGCacheHome  string
	XDGConfigHome string
	TartHome      string
}

func CurrentEnvironment() (Environment, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Environment{}, err
	}
	return Environment{
		GOOS:          runtime.GOOS,
		HomeDir:       home,
		LocalAppData:  os.Getenv("LOCALAPPDATA"),
		AppData:       os.Getenv("APPDATA"),
		XDGStateHome:  os.Getenv("XDG_STATE_HOME"),
		XDGCacheHome:  os.Getenv("XDG_CACHE_HOME"),
		XDGConfigHome: os.Getenv("XDG_CONFIG_HOME"),
		TartHome:      os.Getenv("TART_HOME"),
	}, nil
}

// DockerSandboxesRoots returns every documented persistent root. Linux uses
// independent XDG state, cache, and configuration roots.
func DockerSandboxesRoots(environment Environment) ([]Resolution, error) {
	switch environment.GOOS {
	case "windows":
		if !isAbs("windows", environment.LocalAppData) {
			return nil, errors.New("LOCALAPPDATA is unavailable or not absolute")
		}
		return []Resolution{{ID: "state", Path: join("windows", environment.LocalAppData, "DockerSandboxes"), Provenance: ProvenancePlatformDefault, Confidence: ConfidenceDerived}}, nil
	case "darwin":
		if !isAbs("darwin", environment.HomeDir) {
			return nil, errors.New("home directory is unavailable or not absolute")
		}
		return []Resolution{{ID: "state", Path: join("darwin", environment.HomeDir, "Library", "Application Support", "com.docker.sandboxes"), Provenance: ProvenancePlatformDefault, Confidence: ConfidenceDerived}}, nil
	case "linux":
		if !isAbs("linux", environment.HomeDir) {
			return nil, errors.New("home directory is unavailable or not absolute")
		}
		state, err := xdgRoot("XDG_STATE_HOME", environment.XDGStateHome, join("linux", environment.HomeDir, ".local", "state"))
		if err != nil {
			return nil, err
		}
		cache, err := xdgRoot("XDG_CACHE_HOME", environment.XDGCacheHome, join("linux", environment.HomeDir, ".cache"))
		if err != nil {
			return nil, err
		}
		configuration, err := xdgRoot("XDG_CONFIG_HOME", environment.XDGConfigHome, join("linux", environment.HomeDir, ".config"))
		if err != nil {
			return nil, err
		}
		return []Resolution{
			{ID: "state", Path: join("linux", state, "sandboxes"), Provenance: provenanceForOverride(environment.XDGStateHome), Confidence: ConfidenceDerived},
			{ID: "cache", Path: join("linux", cache, "sandboxes"), Provenance: provenanceForOverride(environment.XDGCacheHome), Confidence: ConfidenceDerived},
			{ID: "config", Path: join("linux", configuration, "sandboxes"), Provenance: provenanceForOverride(environment.XDGConfigHome), Confidence: ConfidenceDerived},
		}, nil
	default:
		return nil, fmt.Errorf("Docker Sandboxes storage discovery is unsupported on %s", environment.GOOS)
	}
}

func TartRoot(environment Environment) (Resolution, error) {
	base := environment.TartHome
	provenance := ProvenanceEnvironment
	if base == "" {
		if !isAbs(environment.GOOS, environment.HomeDir) {
			return Resolution{}, errors.New("home directory is unavailable or not absolute")
		}
		base = join(environment.GOOS, environment.HomeDir, ".tart")
		provenance = ProvenancePlatformDefault
	}
	if !isAbs(environment.GOOS, base) {
		return Resolution{}, errors.New("TART_HOME is not an absolute path")
	}
	return Resolution{ID: "runtime", Path: join(environment.GOOS, base, "vms"), Provenance: provenance, Confidence: ConfidenceDerived}, nil
}

func xdgRoot(name, value, fallback string) (string, error) {
	if value == "" {
		return fallback, nil
	}
	if !isAbs("linux", value) {
		return "", fmt.Errorf("%s is not an absolute path", name)
	}
	return clean("linux", value), nil
}

func provenanceForOverride(value string) Provenance {
	if value != "" {
		return ProvenanceEnvironment
	}
	return ProvenancePlatformDefault
}

func isAbs(goos, value string) bool {
	if value == "" {
		return false
	}
	if goos == "windows" {
		normalized := strings.ReplaceAll(value, "/", "\\")
		return len(normalized) >= 3 && ((normalized[0] >= 'A' && normalized[0] <= 'Z') || (normalized[0] >= 'a' && normalized[0] <= 'z')) && normalized[1] == ':' && normalized[2] == '\\' || strings.HasPrefix(normalized, "\\\\")
	}
	return strings.HasPrefix(value, "/")
}

func clean(goos, value string) string {
	if goos != "windows" {
		return path.Clean(value)
	}
	normalized := strings.ReplaceAll(value, "\\", "/")
	cleaned := path.Clean(normalized)
	return strings.ReplaceAll(cleaned, "/", "\\")
}

func join(goos string, elements ...string) string {
	if goos != "windows" {
		return path.Join(elements...)
	}
	normalized := make([]string, 0, len(elements))
	for _, element := range elements {
		normalized = append(normalized, strings.ReplaceAll(element, "\\", "/"))
	}
	return strings.ReplaceAll(path.Join(normalized...), "/", "\\")
}

func base(goos, value string) string {
	if goos == "windows" {
		value = strings.ReplaceAll(value, "\\", "/")
	}
	return path.Base(value)
}

func dir(goos, value string) string {
	if goos != "windows" {
		return path.Dir(value)
	}
	normalized := strings.ReplaceAll(value, "\\", "/")
	return strings.ReplaceAll(path.Dir(normalized), "/", "\\")
}

func currentCapacityPath(locator string) (string, error) {
	absolute, err := filepath.Abs(locator)
	if err != nil {
		return "", err
	}
	for {
		info, statErr := os.Stat(absolute)
		if statErr == nil {
			if !info.IsDir() {
				absolute = filepath.Dir(absolute)
				continue
			}
			canonical, evalErr := filepath.EvalSymlinks(absolute)
			if evalErr != nil {
				return "", evalErr
			}
			return filepath.Clean(canonical), nil
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(absolute)
		if parent == absolute {
			return "", statErr
		}
		absolute = parent
	}
}
