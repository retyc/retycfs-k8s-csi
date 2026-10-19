package driver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// addPV creates a PV of the driver for testVolume in class, with or without a node-stage secret
// reference, and the class itself (tenant: with node-stage secrets).
func (e *nodeEnv) addPV(class string, tenant, secretRef bool) {
	e.t.Helper()
	ctx := context.Background()
	if class != "" {
		sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: class}, Provisioner: DriverName}
		if tenant {
			sc.Parameters = map[string]string{ //nolint:gosec // G101: parameter names
				nodeStageSecretParam:                             "retycfs-credentials",
				"csi.storage.k8s.io/node-stage-secret-namespace": "${pvc.namespace}",
			}
		}
		if _, err := e.kube.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{}); err != nil {
			e.t.Fatal(err)
		}
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-static"},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: class,
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: DriverName, VolumeHandle: testVolume,
			}},
		},
	}
	if secretRef {
		pv.Spec.CSI.NodeStageSecretRef = &corev1.SecretReference{Name: "retycfs-credentials", Namespace: "team"}
	}
	if _, err := e.kube.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

// A static PV staged without secrets: the driver's own identity is refused for a PV of a tenant
// class, or one naming a node-stage secret, and used for the others.
func TestStageDefaultIdentityTenantCheck(t *testing.T) {
	for _, tc := range []struct {
		name              string
		class             string
		tenant, secretRef bool
		bound             bool // the volume context names the claim, bound to the PV
		want              codes.Code
		message           string
	}{
		{"tenant class, no secret ref", "retycfs-tenant", true, false, false, codes.FailedPrecondition,
			"StorageClass retycfs-tenant is a tenant class"},
		{"tenant class, found from the claim", "retycfs-tenant", true, false, true, codes.FailedPrecondition,
			"tenant class"},
		{"secret ref, no secrets", "retycfs-rwx", false, true, false, codes.FailedPrecondition,
			"names node-stage secret team/retycfs-credentials"},
		{"global class", "retycfs-rwx", false, false, false, codes.Aborted, ""},
		{"no class", "", false, false, false, codes.Aborted, ""},
		{"class deleted", "", false, false, true, codes.Aborted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newNodeEnv(t)
			e.addPV(tc.class, tc.tenant, tc.secretRef)
			if tc.name == "class deleted" {
				pv, _ := e.kube.CoreV1().PersistentVolumes().Get(context.Background(), "pv-static", metav1.GetOptions{})
				pv.Spec.StorageClassName = "gone"
				if _, err := e.kube.CoreV1().PersistentVolumes().Update(context.Background(), pv,
					metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			req := e.stageRequest(nil)
			req.VolumeContext = map[string]string{AnnotationUID: "1000"}
			if tc.bound {
				pvc := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "team"},
					Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-static"},
				}
				if _, err := e.kube.CoreV1().PersistentVolumeClaims("team").Create(context.Background(), pvc,
					metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				req.VolumeContext[volumeContextPVCName], req.VolumeContext[volumeContextNamespace] = "data", "team"
			}
			_, err := e.s.NodeStageVolume(context.Background(), req)
			if status.Code(err) != tc.want || !strings.Contains(status.Convert(err).Message(), tc.message) {
				t.Fatalf("stage = %v, want %v with %q", err, tc.want, tc.message)
			}
			if tc.want == codes.FailedPrecondition && !e.podGone() {
				t.Fatal("a mount pod was created with the driver's identity")
			}
		})
	}
}

// Tenant secrets in the request: no lookup of the PV at all. The API down while checking: Unavailable.
func TestStageDefaultIdentityTenantCheckAPI(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.kube.PrependReactor("*", "persistentvolumes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	tenant := machineSecrets("tenant-key")
	if c := e.stage(e.stageRequest(tenant)); c != codes.Aborted {
		t.Fatalf("stage with tenant secrets = %v, want Aborted (pod starting)", c)
	}
	e2 := newNodeEnv(t)
	e2.claim(map[string]string{AnnotationUID: "1000"})
	e2.kube.PrependReactor("*", "persistentvolumes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if c := e2.stage(e2.stageRequest(nil)); c != codes.Unavailable {
		t.Fatalf("stage with the API down = %v, want Unavailable", c)
	}
}

// No PV for the handle: the driver's identity is refused. Two PVs on the same dataroom, a global
// one and a tenant one: the tenant one is refused, whichever the listing returns first.
func TestStageDefaultIdentityAllPVs(t *testing.T) {
	e := newNodeEnv(t)
	req := e.stageRequest(nil)
	req.VolumeContext = map[string]string{AnnotationUID: "1000"}
	_, err := e.s.NodeStageVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "no PV") {
		t.Fatalf("stage without a PV = %v, want FailedPrecondition", err)
	}

	e = newNodeEnv(t)
	e.addPV("retycfs-tenant", true, false)
	global := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "a-global"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: DriverName, VolumeHandle: testVolume},
		}},
	}
	if _, err := e.kube.CoreV1().PersistentVolumes().Create(context.Background(), global,
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	req = e.stageRequest(nil)
	req.VolumeContext = map[string]string{AnnotationUID: "1000"}
	_, err = e.s.NodeStageVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "tenant class") {
		t.Fatalf("stage with a global and a tenant PV = %v, want FailedPrecondition", err)
	}
	if !e.podGone() {
		t.Fatal("a mount pod was created with the driver's identity")
	}
}
