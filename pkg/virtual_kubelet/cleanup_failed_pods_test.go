package runpod

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testNodeName = "virtual-runpod"

func makeFailedPod(name, reason string, ageOverTTL time.Duration, withDeletionTimestamp bool) *v1.Pod {
	created := time.Now().Add(-(failedPodTTL + ageOverTTL))
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "app",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: v1.PodSpec{NodeName: testNodeName},
		Status: v1.PodStatus{
			Phase:  v1.PodFailed,
			Reason: reason,
		},
	}
	if withDeletionTimestamp {
		now := metav1.NewTime(time.Now())
		pod.DeletionTimestamp = &now
	}
	return pod
}

// fake.Clientset ignores field selectors on status fields, so seeded pods all come
// back from List; the function's own guards (phase, deletion timestamp, age) must
// still produce the expected per-pod decisions.
func TestCleanupFailedPods(t *testing.T) {
	t.Parallel()

	oldDeployFail := makeFailedPod("zombie-deploy", "RunPodDeploymentFailed", time.Minute, false)
	// Real-world "PodDeleted" lives on the container-level Terminated.Reason, leaving
	// pod.Status.Reason empty — encoded here to confirm we do not rely on it.
	oldRemoteGone := makeFailedPod("zombie-remote", "", time.Minute, false)
	oldOther := makeFailedPod("other-reason", "SomeFutureReason", time.Minute, false)
	youngFail := makeFailedPod("recent-fail", "RunPodDeploymentFailed", -2*failedPodTTL, false)
	terminating := makeFailedPod("terminating", "RunPodDeploymentFailed", time.Minute, true)

	running := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "app", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
		Spec:       v1.PodSpec{NodeName: testNodeName},
		Status:     v1.PodStatus{Phase: v1.PodRunning},
	}

	clientset := fake.NewSimpleClientset(oldDeployFail, oldRemoteGone, oldOther, youngFail, terminating, running)

	p := &Provider{
		nodeName:  testNodeName,
		clientset: clientset,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	p.cleanupFailedPods()

	assertGone(t, clientset, "app", "zombie-deploy")
	assertGone(t, clientset, "app", "zombie-remote")
	assertGone(t, clientset, "app", "other-reason")
	assertAlive(t, clientset, "app", "recent-fail")
	assertAlive(t, clientset, "app", "terminating")
	assertAlive(t, clientset, "app", "running")
}

func TestFailedPodAge_PicksLatestTimestamp(t *testing.T) {
	t.Parallel()

	now := time.Now()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
		Status: v1.PodStatus{
			Conditions: []v1.PodCondition{
				{Type: v1.PodScheduled, LastTransitionTime: metav1.NewTime(now.Add(-30 * time.Minute))},
			},
			ContainerStatuses: []v1.ContainerStatus{
				{State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{FinishedAt: metav1.NewTime(now.Add(-10 * time.Minute))}}},
			},
		},
	}

	require.InDelta(t, (10 * time.Minute).Seconds(), failedPodAge(pod).Seconds(), 5)
}

func TestFailedPodAge_FallsBackToCreationTimestamp(t *testing.T) {
	t.Parallel()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Now().Add(-20 * time.Minute))},
	}
	require.InDelta(t, (20 * time.Minute).Seconds(), failedPodAge(pod).Seconds(), 5)
}

func assertGone(t *testing.T, c *fake.Clientset, ns, name string) {
	t.Helper()
	_, err := c.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	assert.True(t, k8serrors.IsNotFound(err), "pod %s/%s expected gone, got err=%v", ns, name, err)
}

func assertAlive(t *testing.T, c *fake.Clientset, ns, name string) {
	t.Helper()
	_, err := c.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	assert.NoError(t, err, "pod %s/%s expected alive", ns, name)
}
