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
	const podIP = "10.244.17.128"
	status := (&KubedirectServer{}).simulateRefPodStatus(pod, podIP)

	if status.PodIP != podIP {
		t.Fatalf("PodIP = %q, want %q", status.PodIP, podIP)
	}
	if status.HostIP != "" {
		t.Fatalf("HostIP = %q, want empty", status.HostIP)
	}
	if len(status.PodIPs) != 1 || status.PodIPs[0].IP != podIP {
		t.Fatalf("PodIPs = %#v, want [%s]", status.PodIPs, podIP)
	}
	ip := net.ParseIP(status.PodIP)
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		t.Fatalf("PodIP %q is not valid for an EndpointSlice address", status.PodIP)
	}
}

func TestSimulatedPodIPAllocatorUsesUpperHalfAndAssignsUniqueAddresses(t *testing.T) {
	server := &KubedirectServer{}
	server.simulatedPodIPs, _ = newPodIPAllocator("10.244.17.0/24")

	first, err := server.allocateSimulatedPodIP("default/a", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.allocateSimulatedPodIP("default/b", first.String())
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "10.244.17.128" || second.String() != "10.244.17.129" {
		t.Fatalf("simulated PodIPs = %s, %s; want upper-half unique addresses", first, second)
	}
}
