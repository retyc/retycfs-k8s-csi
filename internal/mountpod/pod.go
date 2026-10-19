package mountpod

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"path"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
)

// Labels and annotations of the mount pods and their Secrets. A label value is the exact value when
// it is a valid label value, a truncated and hashed one otherwise (see labelValue): the matching
// annotation always holds the exact value.
const (
	LabelApp       = "app.kubernetes.io/name"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelVolume    = driverName + "/volume"
	LabelNode      = driverName + "/node"
	LabelPVC       = driverName + "/pvc"
	LabelNamespace = driverName + "/namespace"

	// AnnotationConfigHash is the hash of everything a running mount pod cannot change: credentials,
	// cache, owner, mount options, resources and image (see Manager.ConfigHash).
	AnnotationConfigHash = driverName + "/config-hash"
	// AnnotationCLIVersion is the retyc-cli version the pod runs (Config.CLIVersion).
	AnnotationCLIVersion = driverName + "/cli-version"
	// AnnotationSecret names the pod's own Secret (one per pod, with a generated suffix).
	AnnotationSecret = driverName + "/secret"
	// AnnotationStagingPath is the staging path the pod mounts the volume on (kubelet's, from
	// NodeStageVolume): with the pod as the only record of a staged volume, it is what the recovery
	// repairs after a restart.
	AnnotationStagingPath = driverName + "/staging-path"
	// AnnotationSpec holds the mount settings of the pod (owner, options, resources, JSON), from
	// which SpecOf rebuilds the Spec of a pod to recreate without the claim at hand.
	AnnotationSpec = driverName + "/spec"

	appName         = "retycfs-mount"
	managedBy       = "retycfs-k8s-csi"
	namePrefix      = "retycfs-mount-"
	driverName      = "csi.retycfs.com"
	containerName   = "mount"
	metricsPortName = "metrics"
)

// Paths inside the mount pod.
const (
	// podStagingDir is where the parent of the staging path is mounted: the CLI mounts the volume
	// on <podStagingDir>/<base of the staging path>, the staging path itself on the host.
	podStagingDir = "/staging"
	podSpoolDir   = "/spool"
	podConfigDir  = "/config"
	// podMachineDir is where the pod's Secret projects the machine account files (RETYC_MACHINE_DIR).
	podMachineDir = "/machine"
	devFuse       = "/dev/fuse"
)

// Keys of the mount pod's Secret besides the identity files (identity.FileNames), read by the CLI
// from its environment.
const (
	secretRedisURL = "RETYC_CACHE_REDIS_URL" //nolint:gosec // G101: a variable name, not a credential
)

// envConfigDir overrides the CLI's config directory (config.yaml and its state); it wins over
// XDG_CONFIG_HOME.
const envConfigDir = "RETYC_CONFIG_DIR"

// machineFileMode is the mode of the projected identity files. The mount container runs as root,
// which reads them whatever their mode; the CLI refuses a file that "other" can read, and nothing
// else in the pod needs them, so owner read only.
const machineFileMode int32 = 0o400

// Spec is what one volume's mount pod needs on one node. Everything but the PVC fields goes into
// the config hash.
type Spec struct {
	// VolumeID is the dataroom ID (the CSI volume handle); it names the mount point and the spool.
	VolumeID string
	NodeName string
	// PVCName and PVCNamespace label the pod; empty for a static PV without a claim reference.
	PVCName      string
	PVCNamespace string
	// StagingPath is kubelet's staging path, where the pod mounts the volume (see Manager.Ensure for
	// the paths accepted). Recorded on the pod (AnnotationStagingPath) for the recovery.
	StagingPath string

	// Credentials are the machine account the volume is mounted with: the driver's default one, or
	// a copy of the tenant's secret received at stage.
	Credentials identity.Credentials
	Cache       Cache
	// UID and GID own every file of the mount (`--uid`, `--gid`).
	UID, GID uint32
	Options  Options
	// Resources override Config.Resources one request or limit at a time (PVC annotations).
	Resources corev1.ResourceRequirements
}

// Cache selects the CLI's listing cache. An empty RedisURL keeps the CLI default (memory).
type Cache struct {
	RedisURL            string
	HighConfidentiality bool
}

// Options are the `retyc fs mount` options set by the PVC annotations. Zero values pass no flag:
// the CLI applies its own defaults.
type Options struct {
	CacheTTL         time.Duration
	BackgroundDelete bool
	UnsafeWrite      bool
	ReadOnly         bool
	// LocalFile replaces the CLI's patterns of files never sent; nil keeps them, {"none"} drops them.
	LocalFile []string
}

func (s *Spec) validate() error {
	switch {
	case s.NodeName == "":
		return fmt.Errorf("%w: node name is empty", ErrInvalidSpec)
	case s.VolumeID == "" || s.VolumeID == "." || s.VolumeID == ".." || strings.ContainsAny(s.VolumeID, "/\x00"):
		return fmt.Errorf("%w: volume ID %q is not a path component", ErrInvalidSpec, s.VolumeID)
	}
	if err := s.Credentials.Complete(); err != nil {
		return fmt.Errorf("%w for volume %s: %w", ErrInvalidSpec, s.VolumeID, err)
	}

	return nil
}

// Name is the name of the mount pod (and of its Secret) of a volume on a node: stable, so that a
// restarted node plugin finds its pods again, and short whatever the length of the inputs.
func Name(nodeName, volumeID string) string {
	sum := sha256.Sum256([]byte(nodeName + "\x00" + volumeID))

	return namePrefix + hex.EncodeToString(sum[:8])
}

// labelValue returns v when it is a valid label value, otherwise a valid value made of a sanitized
// prefix of v and a hash of v: still selectable, and readable enough for kubectl.
func labelValue(v string) string {
	if len(validation.IsValidLabelValue(v)) == 0 {
		return v
	}
	sum := sha256.Sum256([]byte(v))
	prefix := []byte(v)
	for i, c := range prefix {
		if !isLabelChar(c) {
			prefix[i] = '_'
		}
	}
	const maxPrefix = validation.LabelValueMaxLength - 17 // "-" + 16 hex digits
	prefix = prefix[:min(len(prefix), maxPrefix)]
	head := strings.TrimLeft(string(prefix), "-_.")

	return strings.TrimLeft(head+"-"+hex.EncodeToString(sum[:8]), "-")
}

func isLabelChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}

// configHash covers what a pod cannot change once started. The identity enters through its Key,
// a hash of its keys and machine.json: nothing usable is revealed by the annotation.
func configHash(spec *Spec, resources corev1.ResourceRequirements, image string) string {
	data, _ := json.Marshal(struct {
		Identity  string
		Cache     Cache
		UID, GID  uint32
		Options   Options
		Resources corev1.ResourceRequirements
		Image     string
	}{
		spec.Credentials.Key(), spec.Cache, spec.UID, spec.GID, spec.Options, resources, image,
	})
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:16])
}

// resources merges the spec's requests and limits over the configured ones. A request above its
// limit is lowered to the limit when it is a default: an annotation that lowers a limit alone wins.
// Both set by the spec, the API server refuses the pod and Ensure reports it.
func (m *Manager) resources(spec *Spec) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	for _, l := range []corev1.ResourceList{m.cfg.Resources.Requests, spec.Resources.Requests} {
		for name, q := range l {
			out.Requests[name] = q.DeepCopy()
		}
	}
	for _, l := range []corev1.ResourceList{m.cfg.Resources.Limits, spec.Resources.Limits} {
		for name, q := range l {
			out.Limits[name] = q.DeepCopy()
		}
	}
	for name, limit := range out.Limits {
		_, explicit := spec.Resources.Requests[name]
		if req, ok := out.Requests[name]; ok && !explicit && req.Cmp(limit) > 0 {
			out.Requests[name] = limit.DeepCopy()
		}
	}

	return out
}

// mountArgs is the command line of `retyc fs mount` for a spec. Values are passed as --flag=value,
// so that a pattern starting with "-" is not taken for a flag.
func (m *Manager) mountArgs(spec *Spec) []string {
	args := []string{
		"fs", "mount", "retyc://" + spec.VolumeID, path.Join(podStagingDir, path.Base(spec.StagingPath)),
		"--allow-other",
		"--uid=" + strconv.FormatUint(uint64(spec.UID), 10),
		"--gid=" + strconv.FormatUint(uint64(spec.GID), 10),
		"--spool-dir=" + podSpoolDir,
		"--metrics-addr=0.0.0.0:" + strconv.Itoa(m.cfg.MetricsPort),
		"--drain-timeout=" + m.cfg.DrainTimeout.String(),
	}
	o := spec.Options
	if o.CacheTTL > 0 {
		args = append(args, "--cache-ttl="+o.CacheTTL.String())
	}
	if o.BackgroundDelete {
		args = append(args, "--background-delete")
	}
	if o.UnsafeWrite {
		args = append(args, "--unsafe-write")
	}
	if o.ReadOnly {
		args = append(args, "--read-only")
	}
	for _, p := range o.LocalFile {
		args = append(args, "--local-file="+p)
	}
	if spec.Cache.RedisURL != "" {
		args = append(args, "--cache-store=redis")
		if spec.Cache.HighConfidentiality {
			args = append(args, "--cache-redis-high-confidentiality")
		}
	}

	return args
}

// objectMeta is the metadata shared by a mount pod and its Secret.
func (m *Manager) objectMeta(spec *Spec, hash string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      Name(spec.NodeName, spec.VolumeID),
		Namespace: m.cfg.Namespace,
		Labels: map[string]string{
			LabelApp:       appName,
			LabelManagedBy: managedBy,
			LabelVolume:    labelValue(spec.VolumeID),
			LabelNode:      labelValue(spec.NodeName),
			LabelPVC:       labelValue(spec.PVCName),
			LabelNamespace: labelValue(spec.PVCNamespace),
		},
		Annotations: map[string]string{
			LabelVolume:           spec.VolumeID,
			LabelNode:             spec.NodeName,
			LabelPVC:              spec.PVCName,
			LabelNamespace:        spec.PVCNamespace,
			AnnotationConfigHash:  hash,
			AnnotationCLIVersion:  m.cfg.CLIVersion,
			AnnotationStagingPath: spec.StagingPath,
			AnnotationSpec:        specAnnotation(spec),
		},
	}
}

// storedSpec is the part of a Spec recorded on its pod: neither the credentials (in the pod's
// Secret) nor the cache (node-wide, from the Config of the node plugin).
type storedSpec struct {
	UID       uint32                      `json:"uid"`
	GID       uint32                      `json:"gid"`
	Options   Options                     `json:"options"`
	Resources corev1.ResourceRequirements `json:"resources"`
}

func specAnnotation(spec *Spec) string {
	data, _ := json.Marshal(storedSpec{UID: spec.UID, GID: spec.GID, Options: spec.Options, Resources: spec.Resources})

	return string(data)
}

// buildSecret returns a new Secret for a pod of spec: each pod gets its own, so that a former pod
// never takes the Secret of its successor away when the garbage collector deletes it. It holds the
// files the CLI reads (client.key, age.key, machine.json) under their names, which SpecOf reads back.
func (m *Manager) buildSecret(spec *Spec, hash string) *corev1.Secret {
	data := spec.Credentials.MachineFiles()
	if spec.Cache.RedisURL != "" {
		data[secretRedisURL] = []byte(spec.Cache.RedisURL)
	}

	meta := m.objectMeta(spec, hash)
	meta.Name += "-" + randomSuffix()

	return &corev1.Secret{ObjectMeta: meta, Type: corev1.SecretTypeOpaque, Data: data}
}

// randomSuffix is 8 random lowercase letters and digits.
func randomSuffix() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))] //nolint:gosec // G404: a name, not a secret
	}

	return string(b)
}

func (m *Manager) buildPod(spec *Spec, hash, secretName string) *corev1.Pod {
	meta := m.objectMeta(spec, hash)
	meta.Finalizers = []string{FinalizerInUse}
	meta.Annotations[AnnotationSecret] = secretName
	// The pod holds a FUSE mount used by other pods: autoscalers must not take its node away.
	meta.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"] = "false"
	meta.Annotations["karpenter.sh/do-not-disrupt"] = "true"
	secretEnv := func(key string) corev1.EnvVar {
		return corev1.EnvVar{Name: key, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: key,
		}}}
	}
	// The machine account is the Secret's files, projected in podMachineDir. The config directory
	// is the pod's own, empty: neither the image's nor the host's settings apply.
	env := []corev1.EnvVar{
		{Name: identity.MachineDirKey, Value: podMachineDir},
		{Name: envConfigDir, Value: podConfigDir},
	}
	if spec.Cache.RedisURL != "" {
		env = append(env, secretEnv(secretRedisURL))
	}
	env = append(env, m.cfg.ExtraEnv...)

	hostPath := func(name, p string, t corev1.HostPathType) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: p, Type: &t},
		}}
	}
	bidirectional := corev1.MountPropagationBidirectional
	grace := int64((m.cfg.DrainTimeout + terminationMargin) / time.Second)
	privileged, root, noToken, noLinks := true, int64(0), false, false
	machineFiles := make([]corev1.KeyToPath, 0, len(identity.FileNames))
	for _, name := range identity.FileNames {
		if _, ok := spec.Credentials.MachineFiles()[name]; ok {
			machineFiles = append(machineFiles, corev1.KeyToPath{Key: name, Path: name})
		}
	}
	fileMode := machineFileMode

	return &corev1.Pod{
		ObjectMeta: meta,
		Spec: corev1.PodSpec{
			NodeName:                      spec.NodeName,
			Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			PriorityClassName:             m.cfg.PriorityClassName,
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &grace,
			HostPID:                       m.cfg.HostPID,
			HostAliases:                   m.cfg.HostAliases,
			ImagePullSecrets:              m.cfg.ImagePullSecrets,
			// The CLI talks to the Retyc API only; Service variables would land in its RETYC_* space
			// for a Service named "retyc".
			AutomountServiceAccountToken: &noToken,
			EnableServiceLinks:           &noLinks,
			Containers: []corev1.Container{{
				Name:            containerName,
				Image:           m.cfg.Image,
				ImagePullPolicy: m.cfg.ImagePullPolicy,
				Args:            m.mountArgs(spec),
				Env:             env,
				Resources:       m.resources(spec),
				Ports: []corev1.ContainerPort{{
					Name: metricsPortName, ContainerPort: int32(m.cfg.MetricsPort), //nolint:gosec // G115: a port
					Protocol: corev1.ProtocolTCP,
				}},
				// Ready state for kubectl and PodMonitor users; the node plugin probes /readyz itself.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/readyz", Port: intstr.FromString(metricsPortName),
					}},
					PeriodSeconds: 5, FailureThreshold: 1,
				},
				// The end of the log becomes the termination message when the CLI fails.
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				// The image runs as 1000:1000; mount(2) needs root.
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged, RunAsUser: &root, RunAsGroup: &root},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "staging", MountPath: podStagingDir, MountPropagation: &bidirectional},
					{Name: "spool", MountPath: podSpoolDir},
					{Name: "config", MountPath: podConfigDir},
					{Name: "machine", MountPath: podMachineDir, ReadOnly: true},
					{Name: "fuse", MountPath: devFuse},
				},
			}},
			Volumes: []corev1.Volume{
				// The parent of the staging path, not kubelet's whole plugin directory: the pod sees its own
				// volume only. kubelet creates it before NodeStageVolume (it holds vol_data.json) and
				// removes it once NodeUnstageVolume succeeded, that is once this pod is gone.
				hostPath("staging", path.Dir(spec.StagingPath), corev1.HostPathDirectoryOrCreate),
				// Created 0700 by the Prepare hook: kubelet would create a missing one 0755.
				hostPath("spool", m.SpoolHostPath(spec.VolumeID), corev1.HostPathDirectory),
				{Name: "config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				// The identity files only (Items): not the Redis URL of the same Secret.
				{Name: "machine", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: secretName, Items: machineFiles, DefaultMode: &fileMode,
				}}},
				hostPath("fuse", devFuse, corev1.HostPathCharDev),
			},
		},
	}
}

// SpoolHostPath is the host directory holding the files a volume's mount pod has not sent yet.
func (m *Manager) SpoolHostPath(volumeID string) string {
	return path.Join(m.cfg.SpoolRoot, volumeID)
}
