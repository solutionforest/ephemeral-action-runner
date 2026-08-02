package pool

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const (
	storageCatalogControllerLeaseLifetime = 2 * time.Minute
	storageCatalogControllerLeaseRefresh  = 30 * time.Second
)

func (m *Manager) startStorageCatalogControllerLease() (func(), error) {
	if !m.AutomaticImageLifecycle {
		return func() {}, nil
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		return nil, err
	}
	projectRoot := strings.TrimSpace(m.ProjectRoot)
	if projectRoot == "" {
		projectRoot = "."
	}
	configPath := strings.TrimSpace(m.ConfigPath)
	if configPath == "" {
		configPath = filepath.Join(projectRoot, ".local", "config.yml")
	}
	configuredLimit := strings.TrimSpace(m.Config.Storage.BuildCacheLimit)
	if configuredLimit == "" {
		configuredLimit = "20GiB"
	}
	limit, err := config.ParseByteSize(configuredLimit)
	if err != nil {
		return nil, err
	}
	refresh := func(now time.Time) error {
		_, updateErr := store.WithLock(now, func(value *storagecatalog.Catalog) error {
			record, registerErr := storagecatalog.RegisterConfig(value, projectRoot, configPath, now)
			if registerErr != nil {
				return registerErr
			}
			for index := range value.Configs {
				if value.Configs[index].ID == record.ID {
					value.Configs[index].BuildCacheLimitBytes = uint64(limit)
					break
				}
			}
			return storagecatalog.RefreshControllerLease(value, record.ID, now.Add(storageCatalogControllerLeaseLifetime))
		})
		return updateErr
	}
	if err := refresh(m.currentTime()); err != nil {
		return nil, fmt.Errorf("register EPAR storage controller lease: %w", err)
	}
	leaseContext, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(storageCatalogControllerLeaseRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-leaseContext.Done():
				return
			case <-ticker.C:
				if err := refresh(m.currentTime()); err != nil {
					m.warnf("EPAR storage controller lease refresh warning: %v\n", err)
				}
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			now := m.currentTime()
			_, releaseErr := store.WithLock(now, func(value *storagecatalog.Catalog) error {
				configID, idErr := storagecatalog.ConfigID(projectRoot, configPath)
				if idErr != nil {
					return idErr
				}
				storagecatalog.ReleaseControllerLease(value, configID)
				return nil
			})
			if releaseErr != nil {
				m.warnf("EPAR storage controller lease release warning: %v\n", releaseErr)
			}
		})
	}
	return stop, nil
}
