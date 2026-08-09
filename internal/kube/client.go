// Package kube provides Squire's Kubernetes access: pod identity and Events.
package kube

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type Client struct {
	cs        *kubernetes.Clientset
	namespace string
	nodeLabel string // pod label carrying the Slurm node name; "" means default
}

func NewClient(kubeconfig, namespace, nodeLabel string) (*Client, error) {
	if kubeconfig == "" {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	return &Client{cs: cs, namespace: namespace, nodeLabel: nodeLabel}, nil
}

// SlurmNodeLabel is the pod label the slurm-operator stamps with a worker's Slurm node name.
// GetSlurmNodeName returns pod.Spec.Hostname, and it publishes that same value here.
// Overridable via --pod-hostname-label for clusters that differ.
const SlurmNodeLabel = "nodeset.slinky.slurm.net/pod-hostname"

// NodeMap returns a map of Slurm node name -> worker pod, built by asking the
// API server for every pod carrying the operator's hostname label and reading
// the value it published.
func (c *Client) NodeMap(ctx context.Context) (map[string]corev1.Pod, error) {
	label := c.nodeLabel
	if label == "" {
		label = SlurmNodeLabel
	}
	// A bare key as selector means "has this label" — which is exactly the set
	// of Slurm worker pods, and excludes the controller, restapi, and login
	// pods without having to name any of them.
	list, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: label,
	})
	if err != nil {
		return nil, fmt.Errorf("listing worker pods in %s: %w", c.namespace, err)
	}
	m := make(map[string]corev1.Pod, len(list.Items))
	for _, pod := range list.Items {
		if node := pod.Labels[label]; node != "" {
			m[node] = pod
		}
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("no pods in namespace %q carry label %q: "+
			"is this a Slinky cluster, and is --slurm-namespace correct?",
			c.namespace, label)
	}
	return m, nil
}

// PodNames returns Slurm node name -> pod name, for callers that need only the
// identity mapping and not the pod objects. Keeps report free of a corev1
// dependency.
func (c *Client) PodNames(ctx context.Context) (map[string]string, error) {
	nm, err := c.NodeMap(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(nm))
	for node, pod := range nm {
		out[node] = pod.Name
	}
	return out, nil
}

// EmitZombieEvent records a Warning Event on a worker pod for an idle
// GPU allocation held by the given job.
func (c *Client) EmitZombieEvent(ctx context.Context, pod *corev1.Pod, jobID int, user, detail string) error {
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "squire-", Namespace: pod.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1", Kind: "Pod",
			Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID,
		},
		Type:           corev1.EventTypeWarning,
		Reason:         "GPUAllocationIdle",
		Message:        fmt.Sprintf("slurm job %d (user %s) holds GPUs with no activity: %s", jobID, user, detail),
		Source:         corev1.EventSource{Component: "squire"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if _, err := c.cs.CoreV1().Events(pod.Namespace).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating event on %s: %w", pod.Name, err)
	}
	return nil
}
