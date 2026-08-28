package main

import (
	"context"

	"github.com/vhive-serverless/vhive/ctriface"
)

type vhiveOptions struct {
	snapshotter       string
	hostInterface     string
	networkPoolSize   int
	vethPrefix        string
	clonePrefix       string
	dockerCredentials string
	shimPoolSize      int
}

type vhiveManager struct {
	orchestrator *ctriface.Orchestrator
}

func newVHIVEManager(options vhiveOptions) *vhiveManager {
	return &vhiveManager{orchestrator: ctriface.NewOrchestrator(
		options.snapshotter,
		options.hostInterface,
		ctriface.WithTestModeOn(false),
		ctriface.WithSnapshotMode("disabled"),
		ctriface.WithNetPoolSize(options.networkPoolSize),
		ctriface.WithVethPrefix(options.vethPrefix),
		ctriface.WithClonePrefix(options.clonePrefix),
		ctriface.WithDockerCredentials(options.dockerCredentials),
		ctriface.WithShimPoolSize(options.shimPoolSize),
	)}
}

func (m *vhiveManager) Start(ctx context.Context, image string, environment, args []string) (vmInstance, error) {
	response, _, err := m.orchestrator.StartVMWithEnvironment(ctx, image, environment, args)
	if err != nil {
		return vmInstance{}, err
	}
	return vmInstance{ID: response.VMID, GuestIP: response.GuestIP}, nil
}

func (m *vhiveManager) Stop(ctx context.Context, id string) error {
	return m.orchestrator.StopSingleVM(ctx, id)
}
