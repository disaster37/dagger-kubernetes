package repository

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// k8sPeerResolver discovers raft peers via the Kubernetes API (pod IPs),
// avoiding DNS entirely. Like Vault's go-discover with provider=k8s, it
// queries the API server directly — immune to CoreDNS negative-cache
// poisoning and node-local-dns caching issues.
type k8sPeerResolver struct {
	clientset kubernetes.Interface
	namespace string
	stsName   string
	replicas  int
	raftPort  int
	hostname  string
	podIP     string
}

// NewK8sPeerResolver creates a K8s API-based peer resolver. It discovers
// this pod's own IP from the API server to use as the advertise address.
// Falls back gracefully if the API is unreachable (the caller should fall
// back to dnsPeerResolver).
func NewK8sPeerResolver(clientset kubernetes.Interface, namespace, stsName string, replicas, raftPort int, hostname string) (*k8sPeerResolver, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, hostname, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s resolver: get self pod %s/%s: %w", namespace, hostname, err)
	}
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("k8s resolver: pod %s has no IP assigned yet", hostname)
	}
	return &k8sPeerResolver{
		clientset: clientset,
		namespace: namespace,
		stsName:   stsName,
		replicas:  replicas,
		raftPort:  raftPort,
		hostname:  hostname,
		podIP:     pod.Status.PodIP,
	}, nil
}

// PodIP returns this pod's IP as discovered from the Kubernetes API.
func (r *k8sPeerResolver) PodIP() string { return r.podIP }

// Resolve lists StatefulSet pods by name (<sts>-0, <sts>-1, ...) and
// returns their IPs as peer addresses. Pods not yet running (no IP) are
// skipped — the joinLoop will retry on the next reconciliation tick.
func (r *k8sPeerResolver) Resolve() ([]RaftPeer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	peers := make([]RaftPeer, 0, r.replicas)
	for i := 0; i < r.replicas; i++ {
		podName := fmt.Sprintf("%s-%d", r.stsName, i)
		pod, err := r.clientset.CoreV1().Pods(r.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			continue // pod may not exist yet; joinLoop retries
		}
		if pod.Status.PodIP == "" {
			continue
		}
		peers = append(peers, RaftPeer{
			ID:      pod.Name,
			Address: net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(r.raftPort)),
		})
	}

	// Sort by ordinal for deterministic bootstrap (ordinal 0 is first).
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].ID < peers[j].ID
	})

	return peers, nil
}

// Self returns this pod's peer entry with its IP-based address.
func (r *k8sPeerResolver) Self() (RaftPeer, error) {
	return RaftPeer{
		ID:      r.hostname,
		Address: net.JoinHostPort(r.podIP, strconv.Itoa(r.raftPort)),
	}, nil
}
