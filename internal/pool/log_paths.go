package pool

import (
	"path/filepath"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
)

func (m *Manager) logRoot() string {
	return config.ProjectPath(m.ProjectRoot, m.Config.Logging.Directory)
}

func (m *Manager) instanceLogPath(name, suffix string) string {
	return filepath.Join(m.logRoot(), "instances", name+suffix)
}

func (m *Manager) buildLogPath(name string) string {
	return filepath.Join(m.logRoot(), "builds", name)
}
