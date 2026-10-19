// Command retycfs-k8s-csi is the entrypoint for the CSI Controller plugin (--mode=controller), the
// CSI Node plugin (--mode=node) and the enroller of the machine accounts (--mode=enroller) of the
// RetycFS CSI driver. See doc/architecture.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/driver"
	"github.com/retyc/retycfs-k8s-csi/internal/enroller"
	"github.com/retyc/retycfs-k8s-csi/internal/health"
	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/mount"
	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
	"github.com/retyc/retycfs-k8s-csi/internal/retycclient"
)

func main() {
	var (
		mode     = flag.String("mode", "", "plugin mode: controller | node | enroller")
		endpoint = flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
		retycBin = flag.String("retyc-bin", "retyc", "path to the retyc CLI binary")
		nodeID   = flag.String("node-id", "",
			"Kubernetes name of the node, reported by NodeGetInfo (node mode only, default: $NODE_ID, $NODE_NAME)")
		mountConfig = flag.String("mount-config", "/etc/retycfs-csi/mount.json",
			"JSON configuration of the mount pods: image, resources, drain timeout... (node mode only)")
		namespace = flag.String("namespace", "",
			"the driver's namespace, of the mount pods and of the identities' Secrets (node and enroller modes, "+
				"default: $POD_NAMESPACE)")
		httpEndpoint = flag.String("http-endpoint", ":9808",
			"address for the /healthz (liveness), /readyz (readiness) and /metrics HTTP endpoints; empty disables them")
		identityDir = flag.String("identity-dir", driver.DefaultIdentityDir,
			"directory of the driver's default identity (its machine account Secret, mounted), read at each use")
		defaultOrg = flag.String("default-organization-id", "",
			"Retyc organization of the RetycIdentities without spec.organizationID (enroller mode only)")
		clusterName = flag.String("cluster-name", enroller.DefaultClusterName,
			"prefix of the machines' default display name, \"<cluster name>/<namespace>\" (enroller mode only)")
	)
	klog.InitFlags(nil)
	flag.Parse()

	opts := options{
		mode: *mode, endpoint: *endpoint, retycBin: *retycBin, nodeID: *nodeID,
		mountConfig: *mountConfig, namespace: *namespace, httpEndpoint: *httpEndpoint,
		identityDir: *identityDir, defaultOrg: *defaultOrg, clusterName: *clusterName,
	}
	if err := run(opts); err != nil {
		klog.Fatal(err)
	}
}

type options struct {
	mode, endpoint, retycBin, nodeID, mountConfig, namespace, httpEndpoint, identityDir string
	defaultOrg, clusterName                                                             string
}

// defaultIdentity reads the default identity from dir and fails unless it is complete.
func defaultIdentity(dir string) (*identity.Credentials, error) {
	creds, err := identity.FromDir(dir)
	if err != nil {
		return nil, err
	}
	if err := creds.Complete(); err != nil {
		return nil, fmt.Errorf("default identity in %s: %w", dir, err)
	}

	return creds, nil
}

func run(o options) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if o.mode == "enroller" {
		return runEnroller(ctx, o)
	}

	server := grpc.NewServer()
	// Node mode only: the reconciliation of the mount pods with the mounts.
	var (
		recoverPods func(context.Context)
		watchPods   func(context.Context, time.Duration)
	)
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "retycfs_csi_build_info",
			Help:        "Constant 1, labeled with the driver version and the plugin mode.",
			ConstLabels: prometheus.Labels{"version": driver.DriverVersion, "mode": o.mode},
		}, func() float64 { return 1 }),
	)
	// The cluster-wide default identity is the driver's Secret mounted in o.identityDir, read at
	// each use: it may be absent or awaiting approval at start. Tenants' identities arrive per
	// request through CSI secrets.
	if _, err := defaultIdentity(o.identityDir); err != nil {
		klog.Infof("%v: only StorageClasses with secret parameters work until it is complete", err)
	}

	switch o.mode {
	case "controller":
		retyc := retycclient.New(o.retycBin, nil)
		kube, err := inClusterClient()
		if err != nil {
			return err
		}
		events, flushEvents := eventRecorder(kube, driver.DriverName+"-controller", "")
		controller := &driver.ControllerServer{Retyc: retyc, IdentityDir: o.identityDir, Kube: kube, Events: events}
		csi.RegisterControllerServer(server, controller)
		// After the gRPC server stops: the initial members being added, then their Events.
		defer flushEvents()
		defer controller.Wait()
	case "node":
		node, err := nodeServer(o)
		if err != nil {
			return err
		}
		registry.MustRegister(node.Metrics.Collectors()...)
		recoverPods, watchPods = node.Recover, node.Watch
		// /readyz depends on no mount pod: the node plugin serves even when a volume cannot mount.
		csi.RegisterNodeServer(server, node)
	default:
		return fmt.Errorf("--mode must be \"controller\", \"node\" or \"enroller\", got %q", o.mode)
	}
	csi.RegisterIdentityServer(server, &driver.IdentityServer{})

	if o.httpEndpoint != "" {
		// The endpoint listens on the pod IP for kubelet's probes: bound concurrent scrapes.
		metrics := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
			ErrorHandling:       promhttp.ContinueOnError,
			ErrorLog:            klogPrinter{},
			MaxRequestsInFlight: 2,
		})
		go func() {
			if err := health.ListenAndServe(ctx, o.httpEndpoint, health.Handler(nil, metrics)); err != nil {
				klog.Errorf("HTTP endpoints: %v", err)
			}
		}()
	}

	// Before the CSI socket, so that kubelet's first calls find the mounts repaired; bounded, since
	// it only matters for volumes whose mount pod died while the node plugin was down.
	if recoverPods != nil {
		recoverCtx, cancelRecover := context.WithTimeout(ctx, time.Minute)
		recoverPods(recoverCtx)
		cancelRecover()
		go watchPods(ctx, driver.RecoverInterval)
	}

	listener, err := listen(o.endpoint)
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		klog.Info("shutting down gRPC server")
		server.GracefulStop()
	}()

	klog.Infof("%s plugin listening on %s", o.mode, o.endpoint)

	return server.Serve(listener)
}

// nodeServer builds the node plugin: its mount pods manager from --mount-config, the node name and
// the driver namespace from the downward API.
func nodeServer(o options) (*driver.NodeServer, error) {
	nodeID := o.nodeID
	for _, v := range []string{"NODE_ID", "NODE_NAME"} { // the downward API's spec.nodeName
		if nodeID == "" {
			nodeID = os.Getenv(v)
		}
	}
	if nodeID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("determining node ID: %w", err)
		}
		nodeID = hostname
	}
	namespace, err := driverNamespace(o)
	if err != nil {
		return nil, err
	}
	cfg, cache, err := mountpod.LoadConfig(o.mountConfig)
	if err != nil {
		return nil, err
	}
	cfg.Namespace = namespace
	// From a Secret, since a Redis URL usually holds a password.
	if url := os.Getenv("RETYC_CACHE_REDIS_URL"); url != "" {
		cache.RedisURL = url
	}
	kube, err := inClusterClient()
	if err != nil {
		return nil, err
	}
	pods := mountpod.New(kube, cfg)
	events, _ := eventRecorder(kube, driver.DriverName+"-node", nodeID)
	node := &driver.NodeServer{
		NodeID:      nodeID,
		Mounter:     mount.New(),
		Pods:        pods,
		Kube:        kube,
		Events:      events,
		IdentityDir: o.identityDir,
		Cache:       cache,
		Metrics:     driver.NewNodeMetrics(),
	}
	pods.Prepare = node.Prepare
	klog.Infof("node %s: mount pods in namespace %s, image %s", nodeID, namespace, pods.Config().Image)

	return node, nil
}

// driverNamespace is --namespace, or the pod's from the downward API.
func driverNamespace(o options) (string, error) {
	namespace := o.namespace
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	if namespace == "" {
		return "", errors.New("the driver namespace is unknown: set --namespace or $POD_NAMESPACE")
	}

	return namespace, nil
}

// runEnroller runs the enroller of the RetycIdentities until ctx is done, with /healthz and
// /metrics. A single replica: it has no leader election.
func runEnroller(ctx context.Context, o options) error {
	namespace, err := driverNamespace(o)
	if err != nil {
		return err
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("loading the in-cluster Kubernetes config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("creating the Kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("creating the dynamic Kubernetes client: %w", err)
	}
	retyc := retycclient.New(o.retycBin, nil)
	events, flushEvents := eventRecorder(kube, driver.DriverName+"-enroller", "")
	defer flushEvents()
	e := &enroller.Enroller{
		Kube: kube, Dynamic: dyn, Retyc: retyc,
		Auth: func(ctx context.Context, creds *identity.Credentials) error {
			return retyc.WithIdentity(creds).AuthStatusCheck(ctx)
		},
		Events:                events,
		Namespace:             namespace,
		DefaultOrganizationID: o.defaultOrg,
		ClusterName:           o.clusterName,
	}
	if o.httpEndpoint != "" {
		registry := prometheus.NewRegistry()
		registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		metrics := promhttp.HandlerFor(registry, promhttp.HandlerOpts{ErrorLog: klogPrinter{}, MaxRequestsInFlight: 2})
		go func() {
			if err := health.ListenAndServe(ctx, o.httpEndpoint, health.Handler(nil, metrics)); err != nil {
				klog.Errorf("HTTP endpoints: %v", err)
			}
		}()
	}
	klog.Infof("enroller: namespace %s, default organization %q, cluster name %q", namespace, o.defaultOrg,
		o.clusterName)

	return e.Run(ctx)
}

// eventRecorder records events as component (on host, "" for none) through the API server.
// The returned function stops the broadcaster once the Events already queued are handed over.
func eventRecorder(kube kubernetes.Interface, component, host string) (record.EventRecorder, func()) {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})

	return broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: component, Host: host}),
		broadcaster.Shutdown
}

// inClusterClient builds a Kubernetes client from the pod's service account. The controller reads
// the claims it provisions volumes for (their annotations); the node plugin reads the claims it
// stages and manages the mount pods.
func inClusterClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading the in-cluster Kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating the Kubernetes client: %w", err)
	}

	return client, nil
}

// klogPrinter logs the errors promhttp continues past (a family dropped as inconsistent) as warnings.
type klogPrinter struct{}

func (klogPrinter) Println(v ...any) { klog.Warning(v...) }

// listen parses a unix:// CSI endpoint and removes any stale socket file before binding.
func listen(endpoint string) (net.Listener, error) {
	addr, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok {
		return nil, fmt.Errorf("unsupported endpoint scheme %q: only unix:// is supported", endpoint)
	}

	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket %s: %w", addr, err)
	}

	listener, err := net.Listen("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}

	return listener, nil
}
