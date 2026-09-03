package main

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/vhive-serverless/vhive/ctriface"
	"github.com/vhive-serverless/vhive/metrics"
	"github.com/vhive-serverless/vhive/snapshotting"
)

type fakeVHIVEOrchestrator struct {
	mu        sync.Mutex
	events    []string
	startResp *ctriface.StartVMResponse
	startErr  error
	loadErr   error
	createErr error
}

func (o *fakeVHIVEOrchestrator) record(event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *fakeVHIVEOrchestrator) StartVMWithEnvironment(context.Context, string, []string, []string) (*ctriface.StartVMResponse, *metrics.Metric, error) {
	o.record("start")
	return o.startResp, nil, o.startErr
}

func (o *fakeVHIVEOrchestrator) LoadSnapshot(context.Context, *snapshotting.Snapshot, bool, bool) (*ctriface.StartVMResponse, *metrics.Metric, error) {
	o.record("load")
	return o.startResp, nil, o.loadErr
}

func (o *fakeVHIVEOrchestrator) PauseVM(context.Context, string) error {
	o.record("pause")
	return nil
}

func (o *fakeVHIVEOrchestrator) CreateSnapshot(context.Context, string, *snapshotting.Snapshot) error {
	o.record("create")
	return o.createErr
}

func (o *fakeVHIVEOrchestrator) StopSingleVM(context.Context, string) error {
	o.record("stop")
	return nil
}

func (o *fakeVHIVEOrchestrator) Cleanup() {
	o.record("cleanup")
}

type fakeSnapshotManager struct {
	mu     sync.Mutex
	events *[]string
	snap   *snapshotting.Snapshot
	ready  bool
}

func (m *fakeSnapshotManager) record(event string) {
	if m.events != nil {
		*m.events = append(*m.events, event)
	}
}

func (m *fakeSnapshotManager) AcquireSnapshot(string) (*snapshotting.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snap == nil || !m.ready {
		return nil, errors.New("snapshot unavailable")
	}
	return m.snap, nil
}

func (m *fakeSnapshotManager) InitSnapshot(id, image string) (*snapshotting.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("init")
	if m.snap != nil {
		return m.snap, errors.New("snapshot already exists")
	}
	m.snap = snapshotting.NewSnapshot(id, "/tmp", image)
	return m.snap, nil
}

func (m *fakeSnapshotManager) CommitSnapshot(string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("commit")
	m.ready = true
	return nil
}

func (m *fakeSnapshotManager) DeleteSnapshot(string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("delete")
	m.snap = nil
	m.ready = false
	return nil
}

func TestVHIVEManagerStartsFromSnapshotWhenAvailable(t *testing.T) {
	snapshots := &fakeSnapshotManager{
		snap:  snapshotting.NewSnapshot(sharedSnapshotID, "/tmp", vhiveFunctionImage),
		ready: true,
	}
	orchestrator := &fakeVHIVEOrchestrator{
		startResp: &ctriface.StartVMResponse{VMID: "vm-restored", GuestIP: "172.18.0.2"},
	}
	manager := &vhiveManager{orchestrator: orchestrator, snapshots: snapshots}

	instance, err := manager.Start(context.Background(), vhiveFunctionImage, []string{"PORT=80"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "vm-restored" || instance.GuestIP != "172.18.0.2" {
		t.Fatalf("restored instance = %+v", instance)
	}
	if !reflect.DeepEqual(orchestrator.events, []string{"load"}) {
		t.Fatalf("orchestrator events = %v, want snapshot load", orchestrator.events)
	}
}

func TestVHIVEManagerStartsFromImageWithoutSnapshot(t *testing.T) {
	orchestrator := &fakeVHIVEOrchestrator{
		startResp: &ctriface.StartVMResponse{VMID: "vm-new", GuestIP: "172.18.0.3"},
	}
	manager := &vhiveManager{orchestrator: orchestrator, snapshots: &fakeSnapshotManager{}}

	if _, err := manager.Start(context.Background(), vhiveFunctionImage, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orchestrator.events, []string{"start"}) {
		t.Fatalf("orchestrator events = %v, want image start", orchestrator.events)
	}
}

func TestVHIVEManagerCreatesMissingSnapshotBeforeStop(t *testing.T) {
	var snapshotEvents []string
	snapshots := &fakeSnapshotManager{events: &snapshotEvents}
	orchestrator := &fakeVHIVEOrchestrator{}
	manager := &vhiveManager{orchestrator: orchestrator, snapshots: snapshots}

	if err := manager.Stop(context.Background(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	if !snapshots.ready {
		t.Fatal("snapshot was not committed")
	}
	if !reflect.DeepEqual(snapshotEvents, []string{"init", "commit"}) {
		t.Fatalf("snapshot events = %v", snapshotEvents)
	}
	if !reflect.DeepEqual(orchestrator.events, []string{"pause", "create", "stop"}) {
		t.Fatalf("orchestrator events = %v", orchestrator.events)
	}

	if err := manager.Stop(context.Background(), "vm-2"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orchestrator.events, []string{"pause", "create", "stop", "stop"}) {
		t.Fatalf("existing snapshot should skip creation; events = %v", orchestrator.events)
	}
}

func TestVHIVEManagerShutdownCallsCleanupOnce(t *testing.T) {
	orchestrator := &fakeVHIVEOrchestrator{}
	manager := &vhiveManager{orchestrator: orchestrator, snapshots: &fakeSnapshotManager{}}

	manager.Shutdown()
	manager.Shutdown()

	if !reflect.DeepEqual(orchestrator.events, []string{"cleanup"}) {
		t.Fatalf("orchestrator events = %v, want a single cleanup", orchestrator.events)
	}
}

func TestVHIVEManagerDiscardsFailedSnapshotAndStillStops(t *testing.T) {
	var snapshotEvents []string
	snapshots := &fakeSnapshotManager{events: &snapshotEvents}
	orchestrator := &fakeVHIVEOrchestrator{createErr: errors.New("create failed")}
	manager := &vhiveManager{orchestrator: orchestrator, snapshots: snapshots}

	err := manager.Stop(context.Background(), "vm-1")
	if err == nil || !errors.Is(err, orchestrator.createErr) {
		t.Fatalf("Stop error = %v, want create failure", err)
	}
	if snapshots.snap != nil {
		t.Fatal("failed snapshot was retained")
	}
	if !reflect.DeepEqual(snapshotEvents, []string{"init", "delete"}) {
		t.Fatalf("snapshot events = %v", snapshotEvents)
	}
	if !reflect.DeepEqual(orchestrator.events, []string{"pause", "create", "stop"}) {
		t.Fatalf("orchestrator events = %v", orchestrator.events)
	}
}
