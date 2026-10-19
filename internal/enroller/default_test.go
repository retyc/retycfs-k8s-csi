package enroller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEnsureClusterIdentity(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t, nil)
	if created, err := v.e.EnsureClusterIdentity(ctx); created || err != nil {
		t.Fatalf("without a default organization: created %v, %v", created, err)
	}
	if _, err := v.e.resource(driverNS).Get(ctx, ObjectName, metav1.GetOptions{}); err == nil {
		t.Fatal("created without a default organization")
	}

	v.e.DefaultOrganizationID = org
	if created, err := v.e.EnsureClusterIdentity(ctx); !created || err != nil {
		t.Fatalf("created %v, %v", created, err)
	}
	ri := v.status(t, driverNS)
	if ri.Spec.OrganizationID != org || ri.Labels[labelManagedBy] != managedBy {
		t.Errorf("RetycIdentity %+v", ri)
	}
	// Never changed once there, even with another default organization.
	v.e.DefaultOrganizationID = "0190a7f4-0000-7000-8000-000000000002"
	if created, err := v.e.EnsureClusterIdentity(ctx); created || err != nil {
		t.Fatalf("again: created %v, %v", created, err)
	}
	if got := v.status(t, driverNS).Spec.OrganizationID; got != org {
		t.Errorf("organization changed to %s", got)
	}
}

func TestClusterIdentityRecreated(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t, nil)
	v.e.DefaultOrganizationID = org
	// The pass after a deletion finds it gone and creates it again; a tenant's is not.
	if res := v.e.reconcile(ctx, nil, driverNS, ObjectName); !res.gone {
		t.Errorf("result %+v", res)
	}
	if ri := v.status(t, driverNS); ri.Spec.OrganizationID != org {
		t.Errorf("RetycIdentity %+v", ri)
	}
	if res := v.e.reconcile(ctx, nil, tenantNS, ObjectName); !res.gone {
		t.Errorf("tenant: result %+v", res)
	}
	if _, err := v.e.resource(tenantNS).Get(ctx, ObjectName, metav1.GetOptions{}); err == nil {
		t.Error("tenant RetycIdentity created")
	}
}
