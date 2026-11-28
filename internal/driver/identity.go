// Package driver implements the CSI Identity, Controller and Node gRPC services of the Retyc
// CSI driver. Retyc storage spaces, mounted through retycfs, back an RWX
// filesystem volume — see doc/architecture.md for the design and its trade-offs (no snapshot,
// eventually-consistent shared filesystem). The capacity of a volume is the space's storage
// reservation, enlarged by ControllerExpandVolume.
package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/health"
)

// DriverName is advertised to kubelet/external-provisioner and referenced by the StorageClass's
// `provisioner:` field.
const DriverName = "csi.retycfs.com"

// DriverVersion is bumped on every behavior-affecting release; kubelet logs it on registration.
var DriverVersion = "dev" //nolint:gochecknoglobals // set at build time via -ldflags -X (see Makefile, Dockerfile)

// IdentityServer implements csi.IdentityServer. Shared by both the controller and node plugin
// processes (each runs its own gRPC server on its own unix socket).
type IdentityServer struct {
	csi.UnimplementedIdentityServer
	// Ready is the same mode-specific check that backs /readyz (nil: always ready).
	Ready health.Checker
}

func (s *IdentityServer) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          DriverName,
		VendorVersion: DriverVersion,
	}, nil
}

func (s *IdentityServer) GetPluginCapabilities(
	context.Context, *csi.GetPluginCapabilitiesRequest,
) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			// ControllerExpandVolume only enlarges the reservation on the Retyc side: a volume in
			// use can be expanded (without this, csi-resizer waits for the volume to be unpublished).
			{
				Type: &csi.PluginCapability_VolumeExpansion_{
					VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
						Type: csi.PluginCapability_VolumeExpansion_ONLINE,
					},
				},
			},
		},
	}, nil
}

// Probe reports readiness. It's intentionally a static OK: real health (retyc auth validity,
// retycfs mounts) surfaces through CreateVolume/NodeStageVolume errors instead, since
// there's no cheap, side-effect-free liveness check to run here.
// Probe reports the same readiness as /readyz. Not-ready is a plain `ready: false`, not a gRPC
// error: per the CSI spec an error here means "plugin is unhealthy, restart me", and on the node
// a restart is exactly what we don't want for a transient outage.
func (s *IdentityServer) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	if s.Ready != nil {
		if err := s.Ready(ctx); err != nil {
			klog.V(2).Infof("Probe: not ready: %v", err)

			return &csi.ProbeResponse{Ready: wrapperspb.Bool(false)}, nil
		}
	}

	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
