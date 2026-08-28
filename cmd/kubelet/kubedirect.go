package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/exp/rand"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	// Kubedirect
	benchutil "github.com/tomquartz/kubedirect-bench/pkg/util"
	kdctx "k8s.io/kubedirect/pkg/context"
	kdrpc "k8s.io/kubedirect/pkg/rpc"
	kdproto "k8s.io/kubedirect/pkg/rpc/proto"
	kdutil "k8s.io/kubedirect/pkg/util"
)

func init() {
	rand.Seed(uint64(time.Now().UnixNano()))
}

// simulatedPodIP is a TEST-NET-1 address. Kubernetes rejects loopback and
// link-local endpoint addresses, so simulated pods need a syntactically
// routable PodIP even when the data plane replaces it.
const simulatedPodIP = "192.0.2.1"

func isCurrentSimulatedPodIP(status corev1.PodStatus) bool {
	return status.PodIP == simulatedPodIP
}

// impl kdrpc.Registerer
func (s *KubedirectServer) Register(sr grpc.ServiceRegistrar) {
	kdproto.RegisterKubeletServer(sr, s)
}

func (s *KubedirectServer) GetClient(nodeName string) clientset.Interface {
	c, _ := s.clientPool.Get(nodeName)
	return c
}

func (s *KubedirectServer) DelClient(nodeName string) {
	s.clientPool.Del(nodeName)
}

// impl kdproto.KubeletServer
func (s *KubedirectServer) Handshake(ctx context.Context, req *kdproto.HandshakeRequest) (*kdproto.KubeletHandshakeResponse, error) {
	kdLogger := kdutil.NewLogger(klog.FromContext(ctx)).WithHeader(req.Source + "->Handshake")
	kdLogger.Info(fmt.Sprintf("New epoch from %s to %s: %s", req.Source, req.Destination, req.Epoch))
	s.clientPool.GetOrCreate(req.Destination, func() clientset.Interface {
		return benchutil.NewClientsetOrDie()
	})
	holder := s.serverHub.Lock(req.Source, req.Epoch)
	defer holder.Unlock()
	msg := &kdproto.KubeletHandshakeResponse{
		Epoch: req.Epoch,
		Name:  req.Destination,
		Pods:  s.inMemCache.AsPodInfosProtoOnNode(req.Destination),
	}
	return msg, nil
}

// impl kdproto.KubeletServer
func (s *KubedirectServer) BindPod(ctx context.Context, req *kdproto.PodBindingRequest) (*emptypb.Empty, error) {
	kdLogger := kdutil.NewLogger(klog.FromContext(ctx)).WithHeader(req.Source + "->BindPod")
	// get unnamed pod template
	_, err := kdutil.GetUnnamedTemplateFor(ctx, s.podLister, req.PodInfo.Owner.Namespace, req.PodInfo.Owner.Name, false)
	// err is probably due to:
	// 1. the template pod was deleted, which means the rs is also deleted
	// 2. the template pod is not yet added to the informer cache
	// notify the the sender to let it decide whether to retry
	if err != nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound,
			"error getting template pod for %s/%s: %v", req.PodInfo.Owner.Namespace, req.PodInfo.Owner.Name, err,
		)
	}
	podInfo := kdctx.NewPodInfoFromBindingRequest(req)
	// acquire shared lock on epoch
	holder, err := s.serverHub.RLock(req.Source, req.Epoch)
	if err != nil {
		return nil, grpcstatus.Errorf(grpccodes.InvalidArgument, "%s: %v", kdrpc.EpochMismatchError, err)
	}
	defer holder.RUnlock()
	// check if the pod already exists in in-mem cache
	if _, fresh := s.inMemCache.GetOrCreate(podInfo.Name, func() *kdctx.PodInfo { return podInfo }); !fresh {
		kdLogger.WARN("Pod already exists in in-mem cache, will ignore", "pod", podInfo)
		return &emptypb.Empty{}, nil
	}
	kdLogger.Info("Binding", "pod", podInfo)
	// NOTE: BindPod can be called multiple times for the same pod
	// the previous GetOrCreate check should avoid most duplicate deliveries
	// but they can still happen in case the in-mem cache is flushed by informer event handler and BindPod comes in again.
	// but it is fine because we always respect api pods (i.e., with ResourceVersion) if present
	pending := NewPendingPodFromInMemCache(podInfo)
	s.queue.Add(pending)
	return &emptypb.Empty{}, nil
}

func (s *KubedirectServer) ExposeManagedPod(ctx context.Context, pod *corev1.Pod) {
	logger := klog.FromContext(ctx)
	kdLogger := kdutil.NewLogger(logger).WithHeader("Expose").WithValues("pod", klog.KObj(pod))
	if pod.ResourceVersion != "" {
		kdLogger.WARN("Pod with resource version should not be exposed again")
		return
	}
	start := time.Now()
	tryCreate := func(ctx context.Context) (bool, error) {
		_, err := s.GetClient(pod.Spec.NodeName).CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
		if err == nil {
			kdLogger.Info("Pod exposed", "elapsed", time.Since(start))
			return true, nil
		} else if apierrors.IsAlreadyExists(err) {
			kdLogger.V(2).WARN("Pod already exposed")
			return true, nil
		}
		kdLogger.Error(err, "Failed to expose pod")
		return false, nil
	}
	wait.PollUntilContextCancel(ctx, time.Second, true, tryCreate)
}

func (s *KubedirectServer) getRefPodStatus(pod *corev1.Pod) (*corev1.PodStatus, error) {
	// find the reference pod with matching workload label from workload pool
	workloadSelector := labels.Set{
		WorkloadPoolLabel: pod.Labels["workload"],
	}
	workloadPool, err := s.podLister.Pods(pod.Namespace).List(workloadSelector.AsSelectorPreValidated())
	if err != nil {
		return nil, fmt.Errorf("failed to list pods from workload pool: %v", err)
	}
	readyPods := make([]*corev1.Pod, 0, len(workloadPool))
	for i := range workloadPool {
		pod := workloadPool[i]
		if kdutil.IsPodReady(pod) {
			readyPods = append(readyPods, pod)
		}
	}
	if len(readyPods) == 0 {
		return nil, fmt.Errorf("no ready pod matches the workload")
	}
	// randomly select a ready pod from pool
	refPod := readyPods[rand.Intn(len(readyPods))]
	refStatus := refPod.Status.DeepCopy()
	tweakRefPodStatus(refStatus)
	return refStatus, nil
}

func (s *KubedirectServer) simulateRefPodStatus(pod *corev1.Pod) *corev1.PodStatus {
	// simulate the reference pod status
	refStatus := &corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{
			{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			},
			{
				Type:   corev1.ContainersReady,
				Status: corev1.ConditionTrue,
			},
			{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionTrue,
			},
			{
				Type:   corev1.PodInitialized,
				Status: corev1.ConditionTrue,
			},
		},
		PodIP: simulatedPodIP,
	}
	for i := range pod.Spec.ReadinessGates {
		refStatus.Conditions = append(refStatus.Conditions, corev1.PodCondition{
			Type:   pod.Spec.ReadinessGates[i].ConditionType,
			Status: corev1.ConditionTrue,
		})
	}
	literalTrue := true
	for i := range pod.Spec.Containers {
		status := corev1.ContainerStatus{
			Name:    pod.Spec.Containers[i].Name,
			Image:   pod.Spec.Containers[i].Image,
			Started: &literalTrue,
			Ready:   literalTrue,
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{},
			},
		}
		refStatus.ContainerStatuses = append(refStatus.ContainerStatuses, status)
	}
	for i := range pod.Spec.InitContainers {
		status := corev1.ContainerStatus{
			Name:    pod.Spec.InitContainers[i].Name,
			Image:   pod.Spec.InitContainers[i].Image,
			Started: &literalTrue,
			Ready:   literalTrue,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0,
					Reason:   "Completed",
				},
			},
		}
		refStatus.InitContainerStatuses = append(refStatus.InitContainerStatuses, status)
	}
	tweakRefPodStatus(refStatus)
	return refStatus
}

// tweak timestamp fields in status
func tweakRefPodStatus(refStatus *corev1.PodStatus) {
	now := metav1.Now()
	refStatus.StartTime = &now
	for i := range refStatus.Conditions {
		cond := &refStatus.Conditions[i]
		cond.LastTransitionTime = now
	}
	for i := range refStatus.ContainerStatuses {
		status := &refStatus.ContainerStatuses[i]
		if running := status.State.Running; running != nil {
			running.StartedAt = now
		}
	}
	for i := range refStatus.InitContainerStatuses {
		status := &refStatus.InitContainerStatuses[i]
		if running := status.State.Running; running != nil {
			running.StartedAt = now
		}
		if terminated := status.State.Terminated; terminated != nil {
			terminated.StartedAt = now
			terminated.FinishedAt = now
		}
	}
}

func (s *KubedirectServer) markPodReady(ctx context.Context, pod *corev1.Pod, refStatus *corev1.PodStatus) (*corev1.Pod, error) {
	if s.patch {
		return s.markPodReadyByPatch(ctx, pod, refStatus)
	}
	return s.markPodReadyByUpdate(ctx, pod, refStatus)
}

func (s *KubedirectServer) markPodReadyByUpdate(ctx context.Context, pod *corev1.Pod, refStatus *corev1.PodStatus) (*corev1.Pod, error) {
	logger := klog.FromContext(ctx)
	kdLogger := kdutil.NewLogger(logger).WithHeader("Update").WithValues("pod", klog.KObj(pod))
	pod.Status = *refStatus.DeepCopy()
	start := time.Now()
	updatedPod, err := s.GetClient(pod.Spec.NodeName).CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update status: %v", err)
	}
	kdLogger.Info("Pod marked ready", "elapsed", time.Since(start))
	return updatedPod, nil
}

func (s *KubedirectServer) markPodReadyByPatch(ctx context.Context, pod *corev1.Pod, refStatus *corev1.PodStatus) (*corev1.Pod, error) {
	logger := klog.FromContext(ctx)
	kdLogger := kdutil.NewLogger(logger).WithHeader("Patch").WithValues("pod", klog.KObj(pod))
	patchBytes, err := prepareMergePatchBytesForPodStatus(pod.Namespace, pod.Name, pod.UID, *refStatus)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	updatedPod, err := s.GetClient(pod.Spec.NodeName).CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	if err != nil {
		return nil, fmt.Errorf("failed to patch status %q: %v", patchBytes, err)
	}
	kdLogger.Info("Pod marked ready", "elapsed", time.Since(start))
	return updatedPod, nil
}

func prepareMergePatchBytesForPodStatus(namespace, name string, uid types.UID, newPodStatus corev1.PodStatus) ([]byte, error) {
	patchBytes, err := json.Marshal(corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: uid}, // only put the uid in the new object to ensure it appears in the patch as a precondition
		Status:     newPodStatus,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to Marshal newData for pod %q/%q: %v", namespace, name, err)
	}

	return patchBytes, nil
}

// func prepareStrategicPatchBytesForPodStatus(namespace, name string, uid types.UID, oldPodStatus, newPodStatus corev1.PodStatus) ([]byte, bool, error) {
// 	oldData, err := json.Marshal(corev1.Pod{
// 		Status: oldPodStatus,
// 	})
// 	if err != nil {
// 		return nil, false, fmt.Errorf("failed to Marshal old status for pod %q/%q: %v", namespace, name, err)
// 	}

// 	newData, err := json.Marshal(corev1.Pod{
// 		ObjectMeta: metav1.ObjectMeta{UID: uid}, // only put the uid in the new object to ensure it appears in the patch as a precondition
// 		Status:     newPodStatus,
// 	})
// 	if err != nil {
// 		return nil, false, fmt.Errorf("failed to Marshal new status for pod %q/%q: %v", namespace, name, err)
// 	}

// 	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, corev1.Pod{})
// 	if err != nil {
// 		return nil, false, fmt.Errorf("failed to CreateTwoWayMergePatch for pod %q/%q: %v", namespace, name, err)
// 	}
// 	return patchBytes, bytes.Equal(patchBytes, []byte(fmt.Sprintf(`{"metadata":{"uid":%q}}`, uid))), nil
// }
