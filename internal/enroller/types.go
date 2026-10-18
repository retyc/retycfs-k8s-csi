// Package enroller enrolls the driver's Retyc machine accounts from RetycIdentity objects: one in
// the driver's namespace for the cluster-wide identity, one per tenant namespace for the tenants'.
// It runs `retyc machine enroll`, keeps the keys and records in a Secret of the driver's namespace
// from the pending event on, and watches the approved machines with `retyc auth status`. It never
// changes a machine on the Retyc side.
package enroller

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GVR is the resource of the RetycIdentity objects.
var GVR = schema.GroupVersionResource{Group: "csi.retycfs.com", Version: "v1alpha1", Resource: "retycidentities"}

// Kind of the RetycIdentity objects.
const Kind = "RetycIdentity"

// ObjectName is the only name of a RetycIdentity the enroller acts on (the CRD enforces it): one
// identity per namespace.
const ObjectName = "default"

// Phases of a RetycIdentity.
const (
	PhasePending  = "Pending"
	PhaseApproved = "Approved"
	PhaseDenied   = "Denied"
	PhaseExpired  = "Expired"
	PhaseRejected = "Rejected"
	PhaseBlocked  = "Blocked"
)

// Conditions of a RetycIdentity and their reasons.
const (
	// ConditionEnrolled tells where the enrollment stands; its reason drives the retries.
	ConditionEnrolled = "Enrolled"
	// ConditionActive is the verdict of `retyc auth status` on an approved machine.
	ConditionActive = "Active"

	ReasonPending         = "EnrollmentPending"
	ReasonApproved        = "Approved"
	ReasonDenied          = "Denied"
	ReasonExpired         = "Expired"
	ReasonConfigRefused   = "ConfigRefused"
	ReasonNoOrganization  = "NoOrganization"
	ReasonBlocked         = "Blocked"
	ReasonEnrollFailed    = "EnrollFailed"
	ReasonAuthenticated   = "Authenticated"
	ReasonMachineRefused  = "MachineRefused"
	ReasonAuthCheckFailed = "AuthCheckFailed"

	// Events' reasons.
	ReasonClusterIdentityReplaced = "ClusterIdentityReplaced"
	ReasonEnrollmentAbandoned     = "EnrollmentAbandoned"
)

// RetycIdentity asks the enroller for a Retyc machine account for its namespace.
type RetycIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Spec   `json:"spec,omitempty"`
	Status Status `json:"status,omitempty"`
}

// Spec of a RetycIdentity.
type Spec struct {
	// OrganizationID is the Retyc organization to join; the enroller's default when empty.
	OrganizationID string `json:"organizationID,omitempty"`
	// DisplayName is the machine's name in Retyc; "<cluster name>/<namespace>" when empty.
	DisplayName string `json:"displayName,omitempty"`
}

// Status of a RetycIdentity.
type Status struct {
	Phase        string       `json:"phase,omitempty"`
	Fingerprint  string       `json:"fingerprint,omitempty"`
	ApprovalURI  string       `json:"approvalURI,omitempty"`
	EnrollmentID string       `json:"enrollmentID,omitempty"`
	ExpiresAt    *metav1.Time `json:"expiresAt,omitempty"`
	MachineID    string       `json:"machineID,omitempty"`
	ClientID     string       `json:"clientID,omitempty"`
	SecretName   string       `json:"secretName,omitempty"`
	// Attempts counts the enrollment requests that expired unanswered since the last retry.
	Attempts           int32 `json:"attempts,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedRetry is the last value of the retry annotation acted on.
	ObservedRetry string             `json:"observedRetry,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
	Message       string             `json:"message,omitempty"`
}

// fromUnstructured decodes a RetycIdentity.
func fromUnstructured(u *unstructured.Unstructured) (*RetycIdentity, error) {
	var ri RetycIdentity
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ri); err != nil {
		return nil, fmt.Errorf("decoding RetycIdentity %s/%s: %w", u.GetNamespace(), u.GetName(), err)
	}

	return &ri, nil
}

// statusMap encodes a status for an unstructured object.
func statusMap(st *Status) (map[string]any, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(st)
}

// DeepCopy copies a status.
func (s *Status) DeepCopy() *Status {
	out := *s
	if s.ExpiresAt != nil {
		t := *s.ExpiresAt
		out.ExpiresAt = &t
	}
	out.Conditions = nil
	for _, c := range s.Conditions {
		out.Conditions = append(out.Conditions, *c.DeepCopy())
	}

	return &out
}
