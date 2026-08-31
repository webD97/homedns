package peercache

import (
	"context"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	// podIPEnv is supplied by the chart through the downward API. It is both
	// what the probe listener binds to and how this replica excludes itself
	// from its own peer set.
	podIPEnv = "POD_IP"

	// namespaceFile is mounted with the service account token. Reading it
	// avoids a second downward-API env var.
	namespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	resyncPeriod = 10 * time.Minute
)

// discover keeps the peer set in step with the running pods.
//
// Cluster DNS is unusable here -- the Deployment sets dnsPolicy: Default, so a
// headless Service name would not resolve from inside the pod. The API server
// is reachable regardless because InClusterConfig builds its URL from
// KUBERNETES_SERVICE_HOST, never from a name.
//
// Every failure is logged and tolerated. An empty peer set means each query
// takes the upstream leg alone, which is exactly the behaviour without this
// plugin; refusing to start would take DNS away from the whole house because
// the API server blipped.
func (p *PeerCache) discover(ctx context.Context, self string) {
	defer p.wg.Done()

	ns, err := os.ReadFile(namespaceFile)
	if err != nil {
		log.Warningf("reading %s: %v; peer discovery is disabled and every query will take the "+
			"upstream leg alone", namespaceFile, err)
		return
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Warningf("building in-cluster config: %v; peer discovery is disabled", err)
		return
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Warningf("building Kubernetes client: %v; peer discovery is disabled", err)
		return
	}

	factory := informers.NewSharedInformerFactoryWithOptions(client, resyncPeriod,
		informers.WithNamespace(strings.TrimSpace(string(ns))),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = p.selector }),
	)
	pods := factory.Core().V1().Pods()
	lister := pods.Lister()

	republish := func() { p.republish(lister, self) }
	if _, err := pods.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { republish() },
		UpdateFunc: func(any, any) { republish() },
		DeleteFunc: func(any) { republish() },
	}); err != nil {
		log.Warningf("watching pods: %v; peer discovery is disabled", err)
		return
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	republish()

	<-ctx.Done()
	factory.Shutdown()
}

// republish swaps in a whole new peer set so readers never take a lock.
//
// Readiness is deliberately not a filter: a sibling still loading its blocklist
// can answer probes from its store, and warming it is worth doing before it
// starts taking client traffic.
func (p *PeerCache) republish(lister listersv1.PodLister, self string) {
	// The informer only holds pods matching the configured selector, so
	// Everything() here is already scoped.
	pods, err := lister.List(labels.Everything())
	if err != nil {
		log.Warningf("listing pods: %v; keeping the previous peer set", err)
		return
	}

	addrs := make([]string, 0, len(pods))
	for _, pod := range pods {
		ip := pod.Status.PodIP
		if ip == "" || ip == self || pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}
		addrs = append(addrs, net.JoinHostPort(ip, strconv.Itoa(p.port)))
	}
	slices.Sort(addrs)

	if before := p.peerList(); !slices.Equal(before, addrs) {
		log.Infof("peers: %d (%s)", len(addrs), strings.Join(addrs, " "))
	}
	p.peers.Store(&addrs)
	peersTotal.Set(float64(len(addrs)))
}
