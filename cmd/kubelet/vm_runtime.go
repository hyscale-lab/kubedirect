package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

const (
	supportedPodImage   = "ghcr.io/vhive-serverless/invitro_trace_function:latest"
	vhiveFunctionImage  = "ghcr.io/leokondrashov/invitro_trace_function_firecracker:esgz"
	podServingPort      = 8013
	defaultFunctionPort = 80
)

type vmInstance struct {
	ID      string
	GuestIP string
	Port    int32
}

func (v vmInstance) endpoint() string {
	return net.JoinHostPort(v.GuestIP, strconv.Itoa(int(v.Port)))
}

type vmManager interface {
	Start(context.Context, string, []string, []string) (vmInstance, error)
	Stop(context.Context, string) error
}

type podRedirector interface {
	Add(context.Context, netip.Addr, string) error
	Remove(context.Context, netip.Addr, string) error
}

type podVMEntry struct {
	done        chan struct{}
	creating    bool
	removing    bool
	podIP       netip.Addr
	instance    vmInstance
	ruleRemoved bool
	vmStopped   bool
}

// podVMRuntime owns the portion of the Node PodCIDR reserved for the custom
// kubelet and couples every allocation to a vHive VM and an iptables rule.
type podVMRuntime struct {
	manager    vmManager
	redirector podRedirector

	mu      sync.Mutex
	entries map[string]*podVMEntry
	ips     *podIPAllocator
}

func newPodVMRuntime(manager vmManager, redirector podRedirector) *podVMRuntime {
	return &podVMRuntime{
		manager:    manager,
		redirector: redirector,
		entries:    make(map[string]*podVMEntry),
	}
}

func (r *podVMRuntime) configurePodCIDR(cidr string) error {
	ips, err := newPodIPAllocator(cidr)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ips != nil {
		return errors.New("pod VM runtime is already configured")
	}
	r.ips = ips
	return nil
}

func (r *podVMRuntime) reservePodIP(key, value string) error {
	if value == "" {
		return nil
	}
	ip, err := netip.ParseAddr(value)
	if err != nil {
		return fmt.Errorf("parse existing PodIP %q: %w", value, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ips == nil {
		return errors.New("pod VM runtime has no PodCIDR")
	}
	if !r.ips.contains(ip) {
		// Status left by a simulated or legacy custom kubelet is replaced during
		// reconciliation; it does not consume the vHive-owned range.
		return nil
	}
	return r.ips.reserve(key, ip)
}

func (r *podVMRuntime) ensure(ctx context.Context, key string, pod *corev1.Pod) (netip.Addr, error) {
	spec, err := vmSpecForPod(pod)
	if err != nil {
		return netip.Addr{}, err
	}

	for {
		r.mu.Lock()
		if r.ips == nil {
			r.mu.Unlock()
			return netip.Addr{}, errors.New("pod VM runtime has no PodCIDR")
		}
		if existing := r.entries[key]; existing != nil {
			if existing.creating || existing.removing {
				done := existing.done
				r.mu.Unlock()
				select {
				case <-ctx.Done():
					return netip.Addr{}, ctx.Err()
				case <-done:
				}
				continue
			}
			if !existing.vmStopped && !existing.ruleRemoved {
				ip := existing.podIP
				r.mu.Unlock()
				return ip, nil
			}
			r.mu.Unlock()
			return netip.Addr{}, fmt.Errorf("pod %s has a partially removed VM allocation", key)
		}

		preferred := netip.Addr{}
		if pod.Status.PodIP != "" {
			preferred, _ = netip.ParseAddr(pod.Status.PodIP)
		}
		podIP, err := r.ips.allocate(key, preferred)
		if err != nil {
			r.mu.Unlock()
			return netip.Addr{}, err
		}
		entry := &podVMEntry{done: make(chan struct{}), creating: true, podIP: podIP}
		r.entries[key] = entry
		r.mu.Unlock()

		instance, startErr := r.manager.Start(ctx, spec.image, spec.environment, spec.args)
		if startErr == nil {
			instance.Port = spec.port
			startErr = r.redirector.Add(ctx, podIP, instance.endpoint())
			if startErr != nil {
				startErr = errors.Join(startErr, r.manager.Stop(context.Background(), instance.ID))
			}
		}

		r.mu.Lock()
		entry.creating = false
		entry.instance = instance
		if startErr != nil {
			delete(r.entries, key)
			r.ips.release(key)
		}
		close(entry.done)
		r.mu.Unlock()
		if startErr != nil {
			return netip.Addr{}, fmt.Errorf("create VM for pod %s: %w", key, startErr)
		}
		return podIP, nil
	}
}

func (r *podVMRuntime) remove(ctx context.Context, key string) error {
	for {
		r.mu.Lock()
		entry := r.entries[key]
		if entry == nil {
			if r.ips != nil {
				r.ips.release(key)
			}
			r.mu.Unlock()
			return nil
		}
		if entry.creating || entry.removing {
			done := entry.done
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
			}
			continue
		}
		entry.removing = true
		entry.done = make(chan struct{})
		r.mu.Unlock()

		var removeErr error
		if !entry.ruleRemoved {
			if err := r.redirector.Remove(ctx, entry.podIP, entry.instance.endpoint()); err != nil {
				removeErr = errors.Join(removeErr, err)
			} else {
				entry.ruleRemoved = true
			}
		}
		if !entry.vmStopped {
			if err := r.manager.Stop(ctx, entry.instance.ID); err != nil {
				removeErr = errors.Join(removeErr, err)
			} else {
				entry.vmStopped = true
			}
		}

		r.mu.Lock()
		entry.removing = false
		if entry.ruleRemoved && entry.vmStopped {
			delete(r.entries, key)
			r.ips.release(key)
		}
		close(entry.done)
		r.mu.Unlock()
		if removeErr != nil {
			return fmt.Errorf("remove VM for pod %s: %w", key, removeErr)
		}
		return nil
	}
}

type vmSpec struct {
	image       string
	environment []string
	args        []string
	port        int32
}

func vmSpecForPod(pod *corev1.Pod) (vmSpec, error) {
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if container.Image != supportedPodImage {
			continue
		}
		port := int32(defaultFunctionPort)
		if len(container.Ports) != 0 && container.Ports[0].ContainerPort != 0 {
			port = container.Ports[0].ContainerPort
		}
		environment := make([]string, 0, len(container.Env)+2)
		for _, variable := range container.Env {
			if variable.ValueFrom == nil {
				environment = append(environment, variable.Name+"="+variable.Value)
			}
		}
		environment = setEnvironment(environment, "PORT", strconv.Itoa(int(port)))
		environment = setEnvironment(environment, "FUNC_PORT_ENV", strconv.Itoa(int(port)))
		return vmSpec{
			image:       vhiveFunctionImage,
			environment: environment,
			args:        append([]string(nil), container.Args...),
			port:        port,
		}, nil
	}
	return vmSpec{}, fmt.Errorf("pod %s/%s does not contain supported image %q", pod.Namespace, pod.Name, supportedPodImage)
}

func setEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	for i := range environment {
		if len(environment[i]) >= len(prefix) && environment[i][:len(prefix)] == prefix {
			environment[i] = prefix + value
			return environment
		}
	}
	return append(environment, prefix+value)
}

type podIPAllocator struct {
	start  uint32
	end    uint32
	next   uint32
	owners map[uint32]string
	byKey  map[string]uint32
}

func newPodIPAllocator(value string) (*podIPAllocator, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return nil, fmt.Errorf("parse Node PodCIDR %q: %w", value, err)
	}
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("Node PodCIDR %s is not IPv4", prefix)
	}
	prefix = prefix.Masked()
	if prefix.Bits() < 8 || prefix.Bits() > 29 {
		return nil, fmt.Errorf("Node PodCIDR %s has unsupported size; expected /8 through /29", prefix)
	}
	base := addrUint32(prefix.Addr())
	hostBits := 32 - prefix.Bits()
	start := base + uint32(1)<<(hostBits-1)
	end := base + (uint32(1)<<hostBits - 2)
	return &podIPAllocator{
		start: start, end: end, next: start,
		owners: make(map[uint32]string), byKey: make(map[string]uint32),
	}, nil
}

func (a *podIPAllocator) reserve(key string, ip netip.Addr) error {
	if !a.contains(ip) {
		return fmt.Errorf("PodIP %s is outside the custom kubelet range %s-%s", ip, uint32Addr(a.start), uint32Addr(a.end))
	}
	value := addrUint32(ip)
	if old, ok := a.byKey[key]; ok && old != value {
		return fmt.Errorf("pod %s already owns PodIP %s", key, uint32Addr(old))
	}
	if owner, ok := a.owners[value]; ok && owner != key {
		return fmt.Errorf("PodIP %s is already owned by pod %s", ip, owner)
	}
	a.byKey[key] = value
	a.owners[value] = key
	return nil
}

func (a *podIPAllocator) allocate(key string, preferred netip.Addr) (netip.Addr, error) {
	if value, ok := a.byKey[key]; ok {
		return uint32Addr(value), nil
	}
	if a.contains(preferred) {
		if err := a.reserve(key, preferred); err != nil {
			return netip.Addr{}, err
		}
		return preferred, nil
	}
	candidate := a.next
	for {
		if _, used := a.owners[candidate]; !used {
			a.owners[candidate] = key
			a.byKey[key] = candidate
			a.next = candidate + 1
			if a.next > a.end {
				a.next = a.start
			}
			return uint32Addr(candidate), nil
		}
		candidate++
		if candidate > a.end {
			candidate = a.start
		}
		if candidate == a.next {
			return netip.Addr{}, errors.New("custom kubelet PodCIDR range is exhausted")
		}
	}
}

func (a *podIPAllocator) contains(ip netip.Addr) bool {
	if !ip.Is4() {
		return false
	}
	value := addrUint32(ip)
	return value >= a.start && value <= a.end
}

func (a *podIPAllocator) release(key string) {
	if value, ok := a.byKey[key]; ok {
		delete(a.byKey, key)
		delete(a.owners, value)
	}
}

func addrUint32(addr netip.Addr) uint32 {
	b := addr.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uint32Addr(value uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
}

type iptablesRedirector struct {
	binary string
}

func (r iptablesRedirector) Add(ctx context.Context, podIP netip.Addr, endpoint string) error {
	rule := r.rule(podIP, endpoint)
	check := append([]string{"-w", "5", "-t", "nat", "-C", "PREROUTING"}, rule...)
	exists, err := runIPTablesCheck(ctx, r.binary, check)
	if err != nil {
		return fmt.Errorf("check iptables redirect for %s: %w", podIP, err)
	}
	if exists {
		return nil
	}
	insert := append([]string{"-w", "5", "-t", "nat", "-I", "PREROUTING", "1"}, rule...)
	if output, err := exec.CommandContext(ctx, r.binary, insert...).CombinedOutput(); err != nil {
		return fmt.Errorf("add iptables redirect for %s: %w: %s", podIP, err, output)
	}
	return nil
}

func (r iptablesRedirector) Remove(ctx context.Context, podIP netip.Addr, endpoint string) error {
	rule := r.rule(podIP, endpoint)
	check := append([]string{"-w", "5", "-t", "nat", "-C", "PREROUTING"}, rule...)
	exists, err := runIPTablesCheck(ctx, r.binary, check)
	if err != nil {
		return fmt.Errorf("check iptables redirect for %s: %w", podIP, err)
	}
	if !exists {
		return nil
	}
	remove := append([]string{"-w", "5", "-t", "nat", "-D", "PREROUTING"}, rule...)
	if output, err := exec.CommandContext(ctx, r.binary, remove...).CombinedOutput(); err != nil {
		return fmt.Errorf("remove iptables redirect for %s: %w: %s", podIP, err, output)
	}
	return nil
}

func runIPTablesCheck(ctx context.Context, binary string, args []string) (bool, error) {
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("%w: %s", err, output)
}

func (r iptablesRedirector) rule(podIP netip.Addr, endpoint string) []string {
	return []string{
		"-p", "tcp", "-d", podIP.String(), "--dport", strconv.Itoa(podServingPort),
		"-m", "comment", "--comment", "kubedirect-vm-" + podIP.String(),
		"-j", "DNAT", "--to-destination", endpoint,
	}
}
