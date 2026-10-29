package seeded

func controlPlane(s interface{ ControlPlane() error }) error { return s.ControlPlane() }
