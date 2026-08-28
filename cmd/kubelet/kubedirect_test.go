package main

import (
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

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
