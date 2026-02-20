package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestAddTCPPortWithTarget(t *testing.T) {
	svc := NewServiceBuilder("test", "default").
		AddTCPPortWithTarget("portmap", 111, 10111).
		Build()

	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("Expected 1 port, got %d", len(svc.Spec.Ports))
	}

	p := svc.Spec.Ports[0]
	if p.Name != "portmap" {
		t.Errorf("Expected port name 'portmap', got %q", p.Name)
	}
	if p.Port != 111 {
		t.Errorf("Expected service port 111, got %d", p.Port)
	}
	if p.TargetPort.IntVal != 10111 {
		t.Errorf("Expected target port 10111, got %d", p.TargetPort.IntVal)
	}
	if p.Protocol != corev1.ProtocolTCP {
		t.Errorf("Expected TCP protocol, got %s", p.Protocol)
	}
}

func TestAddTCPPortWithTarget_SamePortAsAddTCPPort(t *testing.T) {
	// When port == targetPort, AddTCPPortWithTarget should behave like AddTCPPort.
	svc := NewServiceBuilder("test", "default").
		AddTCPPortWithTarget("nfs", 12049, 12049).
		Build()

	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("Expected 1 port, got %d", len(svc.Spec.Ports))
	}

	p := svc.Spec.Ports[0]
	if p.Port != p.TargetPort.IntVal {
		t.Errorf("Expected port == targetPort when same value, got %d != %d", p.Port, p.TargetPort.IntVal)
	}
}

func TestAddTCPPortWithTarget_MultiPort(t *testing.T) {
	svc := NewServiceBuilder("test", "default").
		AddTCPPort("nfs", 12049).
		AddTCPPortWithTarget("portmap", 111, 10111).
		Build()

	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("Expected 2 ports, got %d", len(svc.Spec.Ports))
	}

	portMap := make(map[string]corev1.ServicePort)
	for _, p := range svc.Spec.Ports {
		portMap[p.Name] = p
	}

	nfs, ok := portMap["nfs"]
	if !ok {
		t.Fatal("Missing nfs port")
	}
	if nfs.Port != 12049 || nfs.TargetPort.IntVal != 12049 {
		t.Errorf("NFS port mismatch: %d->%d", nfs.Port, nfs.TargetPort.IntVal)
	}

	pm, ok := portMap["portmap"]
	if !ok {
		t.Fatal("Missing portmap port")
	}
	if pm.Port != 111 || pm.TargetPort.IntVal != 10111 {
		t.Errorf("Portmap port mismatch: %d->%d", pm.Port, pm.TargetPort.IntVal)
	}
}
