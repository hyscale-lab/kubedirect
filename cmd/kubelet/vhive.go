package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/vhive-serverless/vhive/ctriface"
	"github.com/vhive-serverless/vhive/metrics"
	"github.com/vhive-serverless/vhive/snapshotting"
)

const sharedSnapshotID = "invitro_trace_function_firecracker"

type vhiveOptions struct {
	snapshotter       string
	hostInterface     string
	networkPoolSize   int
	vethPrefix        string
	clonePrefix       string
	dockerCredentials string
	shimPoolSize      int
	snapshotsStorage  string
	debug             bool
}

type vhiveManager struct {
	orchestrator vhiveOrchestrator
	snapshots    snapshotManager
	snapshotMu   sync.Mutex
}

type vhiveOrchestrator interface {
	StartVMWithEnvironment(context.Context, string, []string, []string) (*ctriface.StartVMResponse, *metrics.Metric, error)
	LoadSnapshot(context.Context, *snapshotting.Snapshot, bool, bool) (*ctriface.StartVMResponse, *metrics.Metric, error)
	PauseVM(context.Context, string) error
	CreateSnapshot(context.Context, string, *snapshotting.Snapshot) error
	StopSingleVM(context.Context, string) error
}

type snapshotManager interface {
	AcquireSnapshot(string) (*snapshotting.Snapshot, error)
	InitSnapshot(string, string) (*snapshotting.Snapshot, error)
	CommitSnapshot(string) error
	DeleteSnapshot(string) error
}

func newVHIVEManager(options vhiveOptions) *vhiveManager {
	if options.debug {
		log.SetLevel(log.DebugLevel)
		log.Debug("vHive debug logging is enabled")
	}
	if options.snapshotsStorage == "" {
		options.snapshotsStorage = defaultSnapshotsStorage()
	}
	orchestrator := ctriface.NewOrchestrator(
		options.snapshotter,
		options.hostInterface,
		ctriface.WithTestModeOn(false),
		ctriface.WithSnapshotMode("local"),
		ctriface.WithSnapshotsStorage(options.snapshotsStorage),
		ctriface.WithNetPoolSize(options.networkPoolSize),
		ctriface.WithVethPrefix(options.vethPrefix),
		ctriface.WithClonePrefix(options.clonePrefix),
		ctriface.WithDockerCredentials(options.dockerCredentials),
		ctriface.WithShimPoolSize(options.shimPoolSize),
	)
	return &vhiveManager{
		orchestrator: orchestrator,
		snapshots:    orchestrator.GetSnapshotManager(),
	}
}

func defaultSnapshotsStorage() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "vhive-snapshots")
	}
	return filepath.Join(home, "snapshots")
}

func (m *vhiveManager) Start(ctx context.Context, image string, environment, args []string) (vmInstance, error) {
	snapshot, snapshotErr := m.snapshots.AcquireSnapshot(sharedSnapshotID)
	var response *ctriface.StartVMResponse
	var err error
	if snapshotErr == nil {
		log.Debugf("Using snapshot %s", sharedSnapshotID)
		response, _, err = m.orchestrator.LoadSnapshot(ctx, snapshot, false, false)
	} else {
		log.Debugf("No snapshot %s available, starting from image", sharedSnapshotID)
		response, _, err = m.orchestrator.StartVMWithEnvironment(ctx, image, environment, args)
	}
	if err != nil {
		return vmInstance{}, err
	}
	return vmInstance{ID: response.VMID, GuestIP: response.GuestIP}, nil
}

func (m *vhiveManager) Stop(ctx context.Context, id string) error {
	// Only one terminating VM may initialize the shared snapshot. Other VMs
	// recheck after it is committed and then proceed directly to shutdown.
	m.snapshotMu.Lock()
	_, acquireErr := m.snapshots.AcquireSnapshot(sharedSnapshotID)
	var snapshotErr error
	if acquireErr != nil {
		log.Debugf("Creating absent snapshot %s from VM %s", sharedSnapshotID, id)
		snapshot, err := m.snapshots.InitSnapshot(sharedSnapshotID, vhiveFunctionImage)
		if err != nil {
			snapshotErr = fmt.Errorf("initialize snapshot: %w", err)
		} else {
			if err = m.orchestrator.PauseVM(ctx, id); err == nil {
				err = m.orchestrator.CreateSnapshot(ctx, id, snapshot)
			}
			if err == nil {
				err = m.snapshots.CommitSnapshot(sharedSnapshotID)
			}
			if err != nil {
				snapshotErr = fmt.Errorf("create snapshot: %w", err)
				if cleanupErr := m.snapshots.DeleteSnapshot(sharedSnapshotID); cleanupErr != nil {
					snapshotErr = errors.Join(snapshotErr, fmt.Errorf("discard incomplete snapshot: %w", cleanupErr))
				}
			} else {
				log.Debugf("Created snapshot %s from VM %s", sharedSnapshotID, id)
			}
		}
	}
	m.snapshotMu.Unlock()

	stopErr := m.orchestrator.StopSingleVM(ctx, id)
	return errors.Join(snapshotErr, stopErr)
}
