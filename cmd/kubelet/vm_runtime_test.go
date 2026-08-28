package main

import (
	"context"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeVMManager struct {
	starts int
	stops  int
	image  string
	env    []string
	args   []string
}

func (m *fakeVMManager) Start(_ context.Context, image string, env, args []string) (vmInstance, error) {
	m.starts++
	m.image = image
	m.env = append([]string(nil), env...)
	m.args = append([]string(nil), args...)
	return vmInstance{ID: "vm-1", GuestIP: "172.18.0.2"}, nil
}

func (m *fakeVMManager) Stop(context.Context, string) error {
	m.stops++
	return nil
}

type fakeRedirector struct {
	adds     int
	removes  int
	podIP    netip.Addr
	endpoint string
}

func (r *fakeRedirector) Add(_ context.Context, podIP netip.Addr, endpoint string) error {
	r.adds++
	r.podIP = podIP
	r.endpoint = endpoint
	return nil
}

func (r *fakeRedirector) Remove(_ context.Context, podIP netip.Addr, endpoint string) error {
	r.removes++
	r.podIP = podIP
	r.endpoint = endpoint
	return nil
}

func TestPodVMRuntimeLifecycle(t *testing.T) {
	manager := &fakeVMManager{}
	redirector := &fakeRedirector{}
	runtime := newPodVMRuntime(manager, redirector)
	if err := runtime.configurePodCIDR("10.244.17.0/24"); err != nil {
		t.Fatal(err)
	}
	pod := supportedPod("pod-a")

	first, err := runtime.ensure(context.Background(), "default/pod-a", pod)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.ensure(context.Background(), "default/pod-a", pod)
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "10.244.17.128" || second != first {
		t.Fatalf("allocated PodIPs = %s and %s", first, second)
	}
	if manager.starts != 1 || redirector.adds != 1 {
		t.Fatalf("starts=%d redirects=%d, want one each", manager.starts, redirector.adds)
	}
	if redirector.endpoint != "172.18.0.2:80" {
		t.Fatalf("endpoint = %q", redirector.endpoint)
	}
	if manager.image != vhiveFunctionImage {
		t.Fatalf("VM image = %q", manager.image)
	}
	if !reflect.DeepEqual(manager.args, []string{"--serve"}) {
		t.Fatalf("VM args = %v", manager.args)
	}
	if !reflect.DeepEqual(manager.env, []string{"ITERATIONS_MULTIPLIER=102", "PORT=80", "FUNC_PORT_ENV=80"}) {
		t.Fatalf("VM environment = %v", manager.env)
	}

	if err := runtime.remove(context.Background(), "default/pod-a"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.remove(context.Background(), "default/pod-a"); err != nil {
		t.Fatal(err)
	}
	if manager.stops != 1 || redirector.removes != 1 {
		t.Fatalf("stops=%d redirect removals=%d, want one each", manager.stops, redirector.removes)
	}
}

func TestPodIPAllocatorUsesUpperHalfAndReusesReleasedAddress(t *testing.T) {
	allocator, err := newPodIPAllocator("10.224.8.0/23")
	if err != nil {
		t.Fatal(err)
	}
	first, err := allocator.allocate("default/a", netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "10.224.9.0" {
		t.Fatalf("first upper-half IP = %s", first)
	}
	allocator.release("default/a")
	if err := allocator.reserve("default/b", first); err != nil {
		t.Fatalf("released address was not reusable: %v", err)
	}
}

func TestPodIPAllocatorReplacesLegacyAddress(t *testing.T) {
	allocator, err := newPodIPAllocator("10.244.17.0/24")
	if err != nil {
		t.Fatal(err)
	}
	legacy := netip.MustParseAddr("192.0.2.1")
	got, err := allocator.allocate("default/a", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "10.244.17.128" {
		t.Fatalf("replacement PodIP = %s", got)
	}
}

func TestVMRuntimeRejectsUnsupportedImage(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unsupported"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "queue-proxy", Image: supportedPodImage},
			{Name: userContainerName, Image: "busybox:latest"},
		}},
	}
	if _, err := vmSpecForPod(pod); err == nil {
		t.Fatal("vmSpecForPod accepted queue-proxy's image instead of checking user-container")
	}
}

func TestSupportedUserImageReferences(t *testing.T) {
	digestImage := supportedImageRepository + "@sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		image string
		want  bool
	}{
		{image: supportedPodImage, want: true},
		{image: digestImage, want: true},
		{image: supportedImageRepository + ":dev", want: false},
		{image: supportedImageRepository + "@sha256:not-a-digest", want: false},
		{image: "ghcr.io/example/invitro_trace_function@sha256:" + strings.Repeat("a", 64), want: false},
	}
	for _, test := range tests {
		if got := isSupportedUserImage(test.image); got != test.want {
			t.Errorf("isSupportedUserImage(%q) = %t, want %t", test.image, got, test.want)
		}
	}
}

func TestNodeIPv4PodCIDR(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-a"},
		Spec: corev1.NodeSpec{
			PodCIDR:  "10.244.3.0/24",
			PodCIDRs: []string{"10.244.3.0/24", "fd00:10:244:3::/64"},
		},
	}
	got, err := nodeIPv4PodCIDR(node)
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.244.3.0/24" {
		t.Fatalf("IPv4 PodCIDR = %q", got)
	}
}

func supportedPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  userContainerName,
			Image: supportedPodImage,
			Ports: []corev1.ContainerPort{{ContainerPort: 80}},
			Env:   []corev1.EnvVar{{Name: "ITERATIONS_MULTIPLIER", Value: "102"}},
			Args:  []string{"--serve"},
		}}},
	}
}
