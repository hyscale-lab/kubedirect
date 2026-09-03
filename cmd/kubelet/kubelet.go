package main

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	// Kubedirect
	kdctx "k8s.io/kubedirect/pkg/context"
	kdrpc "k8s.io/kubedirect/pkg/rpc"
	kdproto "k8s.io/kubedirect/pkg/rpc/proto"
	kdutil "k8s.io/kubedirect/pkg/util"
)

const (
	CustomKubeletServicePort  = ":25010"
	PodLifecycleManagerCustom = "custom"
	nWorkers                  = 64
	WorkloadPoolLabel         = "kubedirect/workload-pool"
	// vmRuntimeShutdownTimeout bounds how long process shutdown waits for
	// every tracked pod's VM to stop and its iptables rule to be removed.
	vmRuntimeShutdownTimeout = 2 * time.Minute
)

type PendingPod struct {
	Namespace string
	Name      string
}

func NewPendingPodFromAPIServer(pod *corev1.Pod) PendingPod {
	return PendingPod{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}
}

func NewPendingPodFromInMemCache(podInfo *kdctx.PodInfo) PendingPod {
	return PendingPod{
		Namespace: podInfo.Namespace,
		Name:      podInfo.Name,
	}
}

func (p *PendingPod) String() string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}

type KubedirectServer struct {
	kdLogger  *kdutil.Logger
	serverHub *kdrpc.ServerHub
	kdproto.UnimplementedKubeletServer
	// k8s client and informer
	initClient clientset.Interface
	clientPool *kdutil.SharedMap[clientset.Interface]
	factory    informers.SharedInformerFactory
	// for listing template/managed pods in rpc handlers
	nodeLister corelisters.NodeLister
	podLister  corelisters.PodLister
	// pod queue
	// NOTE: for the queue to deduplicate, we should pass the struct by value
	queue workqueue.TypedRateLimitingInterface[PendingPod]
	// in-mem pod cache
	// NOTE: unlike the default kubelet, the custom kubelet support kubelet service delegation
	// so multiple nodes can map to a single custom kubelet
	inMemCache *kdctx.PodInfoCache
	// Nodename of this kubelet
	nodeName string
	// delay till pod is ready
	readyDelay time.Duration
	// NOTE: unlike the in-mem cache that only handles managed pods with unique names
	// this timer map also handle k8s-originated pods with possibly duplicate names modulo namespaces
	// so we index with namespace/name
	readyTimers *kdutil.SharedMap[time.Time]
	// whether to bind to real containers. if false, just simulate ready delay
	simulate bool
	// use patch or update to mark pod ready
	patch bool
	// vHive VM and reserved PodIP lifecycle manager. Nil keeps the legacy
	// reference-pod behavior for non-simulated test deployments.
	vmRuntime *podVMRuntime
	// simulatedPodIPs owns the same upper half of the Node PodCIDR as the VM
	// runtime would. Simulated pods still need unique PodIPs so EndpointSlices
	// retain every ready replica.
	simulatedPodIPs   *podIPAllocator
	simulatedPodIPsMu sync.Mutex
}

func NewKubedirectServer(c clientset.Interface, nodeName string) *KubedirectServer {
	ctx := context.TODO()
	logger := klog.FromContext(ctx)
	kdLogger := kdutil.NewLogger(logger)

	factory := informers.NewSharedInformerFactory(c, 0)
	kdServer := &KubedirectServer{
		kdLogger:   kdLogger,
		initClient: c,
		clientPool: kdutil.NewSharedMap[clientset.Interface](),
		factory:    factory,
		nodeLister: factory.Core().V1().Nodes().Lister(),
		podLister:  factory.Core().V1().Pods().Lister(),
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[PendingPod](),
			workqueue.TypedRateLimitingQueueConfig[PendingPod]{Name: "custom_kubelet"},
		),
		nodeName:    nodeName,
		inMemCache:  kdctx.NewPodInfoCache(),
		readyTimers: kdutil.NewSharedMap[time.Time](),
	}
	kdServer.serverHub = kdrpc.NewServerHub(kdServer)

	if _, err := factory.Core().V1().Pods().Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			switch t := obj.(type) {
			case *corev1.Pod:
				// both managed label and persistent label are required to be considered managed
				// when materializing in-mem pods, we always set the persistent label and nodename
				return kdServer.enqueueFilter(t)
			case cache.DeletedFinalStateUnknown:
				if p, ok := t.Obj.(*corev1.Pod); ok {
					return kdServer.enqueueFilter(p)
				}
				kdLogger.WARN(fmt.Sprintf("unable to convert deleted object %T to *corev1.Pod", obj))
				return false
			default:
				kdLogger.WARN(fmt.Sprintf("unable to recognize object %T", obj))
				return false
			}
		},
		// the persisted pod is removed from inMemCache in all cases
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(pod interface{}) {
				kdServer.handlePodEvent(pod, false)
			},
			UpdateFunc: func(oldPod, newPod interface{}) {
				kdServer.handlePodEvent(newPod, false)
			},
			DeleteFunc: func(pod interface{}) {
				kdServer.handlePodEvent(pod, true)
			},
		},
	},
	); err != nil {
		kdLogger.Error(err, "Failed to add pod event handlers")
		return nil
	}

	if _, err := factory.Core().V1().Nodes().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(node interface{}) {
			kdLogger := kdLogger.WithHeader("NodeEvent")
			if node := kdServer.unwrapNodeObj(kdLogger, node); node != nil {
				kdServer.DelClient(node.Name)
			}
		},
	},
	); err != nil {
		kdLogger.Error(err, "Failed to add pod event handlers")
		return nil
	}

	// NOTE: unlike scheduler, we don't need to explicitly handle template pod deletion events
	// the exposed pods will be deleted by system-wide GC and notified through the above handler

	return kdServer
}

func (s *KubedirectServer) WithReadyDelay(delay time.Duration) *KubedirectServer {
	s.readyDelay = delay
	return s
}

func (s *KubedirectServer) Simulate() {
	s.simulate = true
}

func (s *KubedirectServer) UsePatch() {
	s.patch = true
}

func (s *KubedirectServer) WithVMRuntime(runtime *podVMRuntime) *KubedirectServer {
	s.vmRuntime = runtime
	return s
}

// the managed label is not required because this server also handles k8s-originated pods
// NOTE: we cannot directly filter on spec.NodeName because there can be kubelet service delegation
func (s *KubedirectServer) enqueueFilter(pod *corev1.Pod) bool {
	return pod.Spec.NodeName != "" && pod.Labels[kdutil.PodLifecycleManagerLabel] == PodLifecycleManagerCustom
}

func (s *KubedirectServer) isResponsibleFor(pod *corev1.Pod) (bool, error) {
	if pod.Spec.NodeName == "" {
		return false, nil
	}
	if pod.Spec.NodeName == s.nodeName {
		return true, nil
	}
	thisNode, thisErr := s.nodeLister.Get(s.nodeName)
	thatNode, thatErr := s.nodeLister.Get(pod.Spec.NodeName)
	if thisErr != nil || thatErr != nil {
		return false, fmt.Errorf("failed to get node object: %v", utilerrors.NewAggregate([]error{thisErr, thatErr}))
	}
	return thisNode.Annotations[kdrpc.KubeletServiceAddrAnnotation] == thatNode.Annotations[kdrpc.KubeletServiceAddrAnnotation], nil
}

func (s *KubedirectServer) handlePodEvent(obj interface{}, isDelete bool) {
	kdLogger := s.kdLogger.WithHeader("PodEvent")
	pod := s.unwrapPodObj(kdLogger, obj)
	if pod == nil {
		return
	}
	pending := NewPendingPodFromAPIServer(pod)
	if !isDelete {
		s.queue.Add(pending)
	} else {
		s.readyTimers.Del(pending.String())
		if s.simulate {
			s.releaseSimulatedPodIP(pending.String())
		}
		if s.vmRuntime != nil {
			go func() {
				if err := s.vmRuntime.remove(context.Background(), pending.String()); err != nil {
					kdLogger.Error(err, "Failed to remove pod VM", "pod", pending.String())
				}
			}()
		}
	}
	// NOTE: the custom kubelet handles both kd-managed and k8s-originated pods
	// but only managed ones are added to in-mem cache
	if kdutil.IsManaged(pod) && kdutil.IsPersistent(pod) {
		// NOTE: index by pod name
		oldInfo, _ := s.inMemCache.Del(pod.Name)
		if oldInfo != nil && kdLogger.V(2).Enabled() {
			kdLogger.DEBUG(fmt.Sprintf("Seen pod %s, remove from in-mem cache", pod.Name), "old", oldInfo, "new", kdctx.NewPodInfoFromPod(pod))
		}
	}
}

func (s *KubedirectServer) SyncPod(ctx context.Context, pending PendingPod) error {
	logger := klog.FromContext(ctx)
	kdLogger := kdutil.NewLogger(logger).WithHeader("SyncPod").WithValues("pod", pending.String())

	var pod *corev1.Pod
	var isInMem bool
	if apiPod, err := s.podLister.Pods(pending.Namespace).Get(pending.Name); err == nil {
		// we always api pod if present, even if pending is from in-mem cache
		pod = apiPod.DeepCopy()
		isInMem = false
	} else if apierrors.IsNotFound(err) {
		// try instantiate from in-mem cache
		podInfo, _ := s.inMemCache.Get(pending.Name)
		if podInfo == nil {
			kdLogger.V(2).DEBUG("Pod not found in in-mem cache or informer cache, will ignore")
			return nil
		}
		// get unnamed pod template
		template, err := kdutil.GetUnnamedTemplateFor(ctx, s.podLister, podInfo.Namespace, podInfo.OwnerName, true)
		if apierrors.IsNotFound(err) {
			kdLogger.WARN("Template pod not found for in-mem pod, will ignore")
			return nil
		} else if err != nil {
			kdLogger.Error(err, "Failed to get template pod")
			return err
		}
		pod = podInfo.AsPersistentPod(template)
		isInMem = true
	} else {
		kdLogger.Error(err, "Failed to get API pod")
		return err
	}

	// check if the pod is bound to this kubelet
	// NOTE: this step is required because enqueueFilter only checks the lifecycle label
	if ok, err := s.isResponsibleFor(pod); err != nil {
		return err
	} else if !ok {
		return nil
	}

	// check api pod status
	// NOTE: deletion timestamp can only be set on api pods; in-mem pods only occur during creation
	// Tear down VM-backed resources before force-deleting the API object. The
	// delete event provides a second, idempotent cleanup path.
	if pod.DeletionTimestamp != nil {
		kdLogger.V(1).Info("Deleting pod")
		if s.simulate {
			s.releaseSimulatedPodIP(pending.String())
		}
		if s.vmRuntime != nil {
			if err := s.vmRuntime.remove(ctx, pending.String()); err != nil {
				kdLogger.Error(err, "Failed to remove pod VM")
				return err
			}
		}
		if err := s.initClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: new(int64), // Set gracePeriodSeconds to 0 to force delete
		}); err != nil && !apierrors.IsNotFound(err) {
			kdLogger.Error(err, "Failed to delete pod")
			return err
		}
		s.readyTimers.Del(pending.String())
		return nil
	}
	// api pod only
	if !kdutil.IsPodActive(pod) {
		kdLogger.V(2).DEBUG("Skipping inactive pod")
		s.readyTimers.Del(pending.String())
		return nil
	}

	var assignedPodIP netip.Addr
	if s.simulate {
		var err error
		assignedPodIP, err = s.allocateSimulatedPodIP(pending.String(), pod.Status.PodIP)
		if err != nil {
			kdLogger.Error(err, "Failed to allocate simulated pod IP")
			return err
		}
	} else if s.vmRuntime != nil {
		var err error
		assignedPodIP, err = s.vmRuntime.ensure(ctx, pending.String(), pod)
		if err != nil {
			kdLogger.Error(err, "Failed to allocate pod VM")
			return err
		}
	}
	// api pod only
	readyPodIPIsCurrent := !assignedPodIP.IsValid() || pod.Status.PodIP == assignedPodIP.String()
	if kdutil.IsPodReady(pod) && readyPodIPIsCurrent {
		kdLogger.V(2).DEBUG("Skipping ready pod")
		s.readyTimers.Del(pending.String())
		return nil
	}

	// check ready delay
	readyTime, fresh := s.readyTimers.GetOrCreate(pending.String(), func() time.Time {
		return time.Now().Add(s.readyDelay)
	})
	// expose in-mem pod if fresh
	if fresh && isInMem {
		go s.ExposeManagedPod(ctx, pod)
	}
	if waitTime := time.Until(readyTime); waitTime > 0 {
		kdLogger.V(1).DEBUG(fmt.Sprintf("Wait %.2fms til ready", waitTime.Seconds()*1e3))
		s.queue.AddAfter(pending, waitTime)
		return nil
	}

	// update pod status to ready
	// if pod is still in-mem at this point, we need to wait till it is exposed
	if isInMem {
		kdLogger.WARN("In-mem pod not exposed, will update status later")
		// no need for explicit requeue because the informer will do so upon pod creation event
		return nil
	}

	// get reference pod status
	var refStatus *corev1.PodStatus
	if s.simulate || s.vmRuntime != nil {
		refStatus = s.simulateRefPodStatus(pod, assignedPodIP.String())
	} else {
		if ref, err := s.getRefPodStatus(pod); err != nil {
			kdLogger.Error(err, "Failed to get reference pod status")
			return err
		} else {
			refStatus = ref
		}
	}
	// QoSClass is derived from a pod's resource requirements and is immutable.
	// Reference pods may use different requirements, so never copy their QoS
	// class onto the pod whose status is being updated.
	refStatus.QOSClass = pod.Status.QOSClass
	// Reference pods can have a different container layout. In particular,
	// Knative injects queue-proxy into the target pod, while the workload-pool
	// pod has only the function container. Statuses must cover every container
	// in the target pod for Kubernetes to report it fully ready.
	targetStatus := s.simulateRefPodStatus(pod, refStatus.PodIP)
	refStatus.ContainerStatuses = targetStatus.ContainerStatuses
	refStatus.InitContainerStatuses = targetStatus.InitContainerStatuses
	if assignedPodIP.IsValid() {
		refStatus.PodIP = assignedPodIP.String()
		refStatus.PodIPs = []corev1.PodIP{{IP: assignedPodIP.String()}}
	}

	if _, err := s.markPodReady(ctx, pod, refStatus); err != nil {
		kdLogger.Error(err, "Failed to mark pod as ready")
		// notfound/conflict errs would be handled after requeue
		return err
	}
	// readyTimers would be removed once the updated status triggers the informer event handler
	return nil
}

func (s *KubedirectServer) processNextItem(ctx context.Context) bool {
	pending, shutdown := s.queue.Get()
	if shutdown {
		return false
	}
	defer s.queue.Done(pending)

	err := s.SyncPod(ctx, pending)
	if err == nil {
		s.queue.Forget(pending)
		return true
	}
	utilruntime.HandleErrorWithContext(ctx, err, fmt.Sprintf("Erroring syncing %v: %v", pending, err))
	s.queue.AddRateLimited(pending)

	return true
}

func (s *KubedirectServer) workerLoop(ctx context.Context) {
	for s.processNextItem(ctx) {
	}
}

func (s *KubedirectServer) ListenAndServe(ctx context.Context) error {
	defer utilruntime.HandleCrashWithContext(ctx)
	defer s.queue.ShutDown()

	logger := klog.FromContext(ctx)
	kdLogger := kdutil.NewLogger(logger).WithHeader("Main")

	s.factory.Start(ctx.Done())
	for k, ok := range s.factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("error syncing %v", k)
		}
	}

	if s.vmRuntime != nil || s.simulate {
		node, err := s.nodeLister.Get(s.nodeName)
		if err != nil {
			return fmt.Errorf("get node %s for pod IP allocation: %w", s.nodeName, err)
		}
		podCIDR, err := nodeIPv4PodCIDR(node)
		if err != nil {
			return fmt.Errorf("configure pod IP allocation: %w", err)
		}
		if s.vmRuntime != nil {
			if err := s.vmRuntime.configurePodCIDR(podCIDR); err != nil {
				return fmt.Errorf("configure pod VM networking: %w", err)
			}
		}
		if s.simulate {
			ips, err := newPodIPAllocator(podCIDR)
			if err != nil {
				return fmt.Errorf("configure simulated pod IP allocation: %w", err)
			}
			s.simulatedPodIPsMu.Lock()
			s.simulatedPodIPs = ips
			s.simulatedPodIPsMu.Unlock()
		}
		if s.vmRuntime != nil {
			pods, err := s.podLister.List(labels.Everything())
			if err != nil {
				return fmt.Errorf("list existing pods for pod VM networking: %w", err)
			}
			for _, pod := range pods {
				if !s.enqueueFilter(pod) || pod.Status.PodIP == "" {
					continue
				}
				if _, err := vmSpecForPod(pod); err != nil {
					continue
				}
				responsible, err := s.isResponsibleFor(pod)
				if err != nil {
					return err
				}
				if responsible {
					pending := NewPendingPodFromAPIServer(pod)
					key := pending.String()
					if err := s.vmRuntime.reservePodIP(key, pod.Status.PodIP); err != nil {
						return fmt.Errorf("reserve existing PodIP for %s: %w", key, err)
					}
				}
			}
			kdLogger.Info("Configured pod VM networking", "podCIDR", podCIDR)
		} else {
			kdLogger.Info("Configured simulated pod IP allocation", "podCIDR", podCIDR)
		}
	}

	publishServiceAddr := func(ctx context.Context) (bool, error) {
		node, err := s.nodeLister.Get(s.nodeName)
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("node %s not found", s.nodeName)
		}
		var hostIP string
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				hostIP = addr.Address
				break
			}
		}
		if hostIP == "" {
			return false, fmt.Errorf("node %s has no internal IP", s.nodeName)
		}
		node = node.DeepCopy()
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}
		node.Annotations[kdrpc.KubeletServiceAddrAnnotation] = hostIP + CustomKubeletServicePort
		if _, err := s.initClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
			kdLogger.Error(err, fmt.Sprintf("Failed to update node %v", s.nodeName))
			return false, nil
		}
		kdLogger.Info(fmt.Sprintf("Published custom kubelet service address: %s", node.Annotations[kdrpc.KubeletServiceAddrAnnotation]))
		return true, nil
	}
	if err := wait.PollUntilContextCancel(ctx, time.Second, true, publishServiceAddr); err != nil {
		return fmt.Errorf("failed to publish custom kubelet service address: %v", err)
	}

	for i := 0; i < nWorkers; i++ {
		go wait.UntilWithContext(ctx, s.workerLoop, time.Second)
	}

	err := s.serverHub.ListenAndServe(ctx, CustomKubeletServicePort)

	// ctx is already canceled here, so every per-pod teardown below (iptables
	// exec calls, VM stop RPCs) needs its own context that will actually let
	// them run to completion instead of being canceled immediately.
	if s.vmRuntime != nil {
		kdLogger.Info("Removing pod VM network rules and stopping pod VMs")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), vmRuntimeShutdownTimeout)
		defer cancel()
		if shutdownErr := s.vmRuntime.shutdown(shutdownCtx); shutdownErr != nil {
			kdLogger.Error(shutdownErr, "Failed to fully remove pod VM network rules")
		}
	}

	return err
}

func nodeIPv4PodCIDR(node *corev1.Node) (string, error) {
	candidates := append([]string(nil), node.Spec.PodCIDRs...)
	if node.Spec.PodCIDR != "" {
		candidates = append(candidates, node.Spec.PodCIDR)
	}
	var selected string
	for _, candidate := range candidates {
		prefix, err := netip.ParsePrefix(candidate)
		if err != nil {
			return "", fmt.Errorf("parse Node PodCIDR %q: %w", candidate, err)
		}
		if !prefix.Addr().Is4() {
			continue
		}
		if selected != "" && selected != candidate {
			return "", fmt.Errorf("node %s has more than one IPv4 PodCIDR", node.Name)
		}
		selected = candidate
	}
	if selected == "" {
		return "", fmt.Errorf("node %s has no IPv4 PodCIDR", node.Name)
	}
	return selected, nil
}

func (s *KubedirectServer) unwrapPodObj(kdLogger *kdutil.Logger, obj interface{}) *corev1.Pod {
	var pod *corev1.Pod
	switch t := obj.(type) {
	case *corev1.Pod:
		pod = obj.(*corev1.Pod)
	case cache.DeletedFinalStateUnknown:
		var ok bool
		pod, ok = t.Obj.(*corev1.Pod)
		if !ok {
			kdLogger.Error(nil, fmt.Sprintf("Cannot convert %T to *v1.Pod", obj))
			return nil
		}
	default:
		kdLogger.Error(nil, fmt.Sprintf("Cannot convert %T to *v1.Pod", obj))
		return nil
	}
	if !s.enqueueFilter(pod) {
		kdLogger.WARN("Pod does not pass enqueue filter", "pod", klog.KObj(pod))
		return nil
	}
	return pod
}

func (s *KubedirectServer) unwrapNodeObj(kdLogger *kdutil.Logger, obj interface{}) *corev1.Node {
	var node *corev1.Node
	switch t := obj.(type) {
	case *corev1.Node:
		node = obj.(*corev1.Node)
	case cache.DeletedFinalStateUnknown:
		var ok bool
		node, ok = t.Obj.(*corev1.Node)
		if !ok {
			kdLogger.Error(nil, fmt.Sprintf("Cannot convert %T to *v1.Node", obj))
			return nil
		}
	default:
		kdLogger.Error(nil, fmt.Sprintf("Cannot convert %T to *v1.Node", obj))
		return nil
	}
	return node
}
