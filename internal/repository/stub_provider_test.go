package repository

import "testing"

func TestStubProviderPVCLifecycle(t *testing.T) {
	p := NewStubProvider()
	if err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0"); err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}

	// ScaleUp creates the per-pod PVCs, like the StatefulSet controller.
	if err := p.ScaleUp("v0.20.0", 2); err != nil {
		t.Fatalf("ScaleUp: %v", err)
	}
	pvcs, err := p.ListPVCs("v0.20.0")
	if err != nil {
		t.Fatalf("ListPVCs: %v", err)
	}
	if len(pvcs) != 2 || pvcs[0].Ordinal != 0 || pvcs[1].Ordinal != 1 {
		t.Fatalf("pvcs = %+v, want ordinals 0 and 1 sorted", pvcs)
	}

	// ScaleDown retains the PVCs (WhenScaled: Retain).
	if err := p.ScaleDown("v0.20.0", 1); err != nil {
		t.Fatalf("ScaleDown: %v", err)
	}
	if pvcs, _ = p.ListPVCs("v0.20.0"); len(pvcs) != 2 {
		t.Fatalf("pvcs = %+v, want both retained", pvcs)
	}

	// AddPVC seeds an orphaned PVC for tests.
	p.AddPVC("v0.20.0", 5)
	if pvcs, _ = p.ListPVCs("v0.20.0"); len(pvcs) != 3 || pvcs[2].Ordinal != 5 {
		t.Fatalf("pvcs = %+v, want ordinal 5 appended", pvcs)
	}

	// DeletePVC removes it; deleting it again is a no-op (NotFound).
	name := enginePVCName("v0.20.0", 5)
	if err := p.DeletePVC(name); err != nil {
		t.Fatalf("DeletePVC: %v", err)
	}
	if err := p.DeletePVC(name); err != nil {
		t.Fatalf("DeletePVC again: %v", err)
	}
	if pvcs, _ = p.ListPVCs("v0.20.0"); len(pvcs) != 2 {
		t.Fatalf("pvcs = %+v, want the orphan removed", pvcs)
	}
}

func TestStubProviderAddPVCCreatesVersion(t *testing.T) {
	p := NewStubProvider()

	p.AddPVC("v0.21.0", 0)

	pvcs, err := p.ListPVCs("v0.21.0")
	if err != nil {
		t.Fatalf("ListPVCs: %v", err)
	}
	if len(pvcs) != 1 || pvcs[0].Name != enginePVCName("v0.21.0", 0) {
		t.Fatalf("pvcs = %+v, want the seeded PVC", pvcs)
	}
}

func TestStubProviderListPVCsUnknownVersion(t *testing.T) {
	p := NewStubProvider()

	pvcs, err := p.ListPVCs("v0.20.0")
	if err != nil {
		t.Fatalf("ListPVCs: %v", err)
	}
	if len(pvcs) != 0 {
		t.Fatalf("pvcs = %+v, want empty", pvcs)
	}
}

func TestStubProviderDeleteStatefulSetRemovesPVCs(t *testing.T) {
	p := NewStubProvider()
	if err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0"); err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}
	p.AddPVC("v0.20.0", 0)

	if err := p.DeleteStatefulSet("v0.20.0"); err != nil {
		t.Fatalf("DeleteStatefulSet: %v", err)
	}
	pvcs, err := p.ListPVCs("v0.20.0")
	if err != nil {
		t.Fatalf("ListPVCs: %v", err)
	}
	if len(pvcs) != 0 {
		t.Fatalf("pvcs = %+v, want empty after delete", pvcs)
	}
}
