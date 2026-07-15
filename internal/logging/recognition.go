package logging

import (
	"path/filepath"
	"regexp"
	"strings"
)

var (
	lumberjackSuffixPattern = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}[Tt]\d{2}-\d{2}-\d{2}\.\d{3}(\.log|\.jsonl)$`)
	errorNamePattern        = regexp.MustCompile(`^epar-\d{8}-\d{6}-error\.log$`)
	instanceNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.(guest|docker-dind|wsl|tart)\.log$`)
	legacyInstancePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*-\d{8}-\d{6}-\d{3}\.(guest|docker-dind|wsl|tart)\.log$`)
	buildNamePattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.(docker-build|wsl-build|build|source|refresh|wsl-refresh|guest)\.log$`)
	legacyBuildNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.(docker-build|wsl-build|build|source|refresh|wsl-refresh)\.log$`)
	benchmarkNamePattern    = regexp.MustCompile(`^\d{8}[Tt]\d{6}\.\d{9}[Zz]-[A-Za-z0-9][A-Za-z0-9_-]*\.jsonl$`)
)

type recognizedFile struct {
	category Category
	current  bool
	backup   bool
}

func recognizePath(root, path string) (recognizedFile, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(relative) || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return recognizedFile{}, false
	}
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	if len(parts) == 1 {
		return recognizeName(parts[0], "")
	}
	if len(parts) != 2 {
		return recognizedFile{}, false
	}
	category := Category(parts[0])
	if !category.validTranscriptCategory() {
		return recognizedFile{}, false
	}
	recognized, ok := recognizeName(parts[1], category)
	if !ok || recognized.category != category {
		return recognizedFile{}, false
	}
	return recognized, true
}

func recognizeName(name string, expected Category) (recognizedFile, bool) {
	primary, backup := stripLumberjackSuffix(name)
	if expected == "" || expected == CategoryManager {
		if primary == ManagerFilename {
			return recognizedFile{category: CategoryManager, current: !backup, backup: backup}, true
		}
		if primary == LastErrorFilename && !backup {
			return recognizedFile{category: CategoryErrors, current: true}, true
		}
	}
	if expected == "" || expected == CategoryErrors {
		if errorNamePattern.MatchString(primary) {
			return recognizedFile{category: CategoryErrors, backup: backup}, true
		}
	}
	if expected == "" || expected == CategoryBuilds {
		pattern := buildNamePattern
		if expected == "" {
			pattern = legacyBuildNamePattern
		}
		if pattern.MatchString(primary) {
			return recognizedFile{category: CategoryBuilds, backup: backup}, true
		}
	}
	if expected == "" || expected == CategoryBenchmarks {
		if benchmarkNamePattern.MatchString(primary) {
			return recognizedFile{category: CategoryBenchmarks, backup: backup}, true
		}
	}
	if expected == "" || expected == CategoryInstances {
		pattern := instanceNamePattern
		if expected == "" {
			pattern = legacyInstancePattern
		}
		if pattern.MatchString(primary) {
			return recognizedFile{category: CategoryInstances, backup: backup}, true
		}
	}
	return recognizedFile{}, false
}

func stripLumberjackSuffix(name string) (string, bool) {
	original := name
	gzipped := strings.HasSuffix(name, ".gz")
	if gzipped {
		name = strings.TrimSuffix(name, ".gz")
	}
	location := lumberjackSuffixPattern.FindStringIndex(name)
	if location == nil || location[1] != len(name) {
		return original, false
	}
	suffix := name[location[0]:]
	extension := ".log"
	if strings.HasSuffix(suffix, ".jsonl") {
		extension = ".jsonl"
	}
	return name[:location[0]] + extension, true
}

func activeLockPathForRecognized(path string, recognized recognizedFile) string {
	if !recognized.backup {
		return path
	}
	primary, ok := stripLumberjackSuffix(filepath.Base(path))
	if !ok {
		return path
	}
	return filepath.Join(filepath.Dir(path), primary)
}
