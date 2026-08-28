package main

import (
	"context"
	"net"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	kdproto "k8s.io/kubedirect/pkg/rpc/proto"
	kdutil "k8s.io/kubedirect/pkg/util"
)

func TestBindPodMaterializesTemplateBeforeVMAllocation(t *testing.T) {
	server := NewKubedirectServer(fake.NewSimpleClientset(), "worker-a")
	defer server.queue.ShutDown()

	manager := &fakeVMManager{}
	runtime := newPodVMRuntime(manager, &fakeRedirector{})
	if err := runtime.configurePodCIDR("10.244.17.0/24"); err != nil {
		t.Fatal(err)
	}
	server.WithVMRuntime(runtime)

	template := supportedPod("trace-template")
	template.Labels = map[string]string{
		kdutil.OwnerNameLabel:   "trace",
		kdutil.TemplatePodLabel: "true",
	}
	if err := server.factory.Core().V1().Pods().Informer().GetStore().Add(template); err != nil {
		t.Fatal(err)
	}
	holder := server.serverHub.Lock("scheduler-a", "epoch-a")
	holder.Unlock()

	_, err := server.BindPod(context.Background(), &kdproto.PodBindingRequest{
		Source:   "scheduler-a",
		Epoch:    "epoch-a",
		NodeName: "worker-a",
		PodInfo: &kdproto.PodInfo{
			Owner:             &kdproto.NamespacedName{Namespace: "default", Name: "trace"},
			Name:              "trace-0",
			CreationTimestamp: time.Now().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if manager.starts != 1 {
		t.Fatalf("VM starts = %d, want 1", manager.starts)
	}
}

func TestSimulateRefPodStatusSetsPodIP(t *testing.T) {
	pod := &corev1.Pod{}
	status := (&KubedirectServer{}).simulateRefPodStatus(pod)

	if status.PodIP != simulatedPodIP {
		t.Fatalf("PodIP = %q, want %q", status.PodIP, simulatedPodIP)
	}
	if status.HostIP != "" {
		t.Fatalf("HostIP = %q, want empty", status.HostIP)
	}
	if len(status.PodIPs) != 0 {
		t.Fatalf("len(PodIPs) = %d, want 0", len(status.PodIPs))
	}
	ip := net.ParseIP(status.PodIP)
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		t.Fatalf("PodIP %q is not valid for an EndpointSlice address", status.PodIP)
	}
	if !isCurrentSimulatedPodIP(*status) {
		t.Fatal("new simulated status was considered stale")
	}
}

func TestCurrentSimulatedPodIP(t *testing.T) {
	tests := []struct {
		name   string
		status corev1.PodStatus
		want   bool
	}{
		{
			name: "current",
			status: corev1.PodStatus{
				PodIP: simulatedPodIP,
			},
			want: true,
		},
		{
			name:   "missing podIP",
			status: corev1.PodStatus{},
		},
		{
			name: "old loopback address",
			status: corev1.PodStatus{
				PodIP: "127.0.0.1",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isCurrentSimulatedPodIP(test.status); got != test.want {
				t.Fatalf("isCurrentSimulatedPodIP() = %t, want %t", got, test.want)
			}
		})
	}
}
