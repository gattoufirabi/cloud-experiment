package model

import "testing"

func TestInstances(t *testing.T) {
	base := Spec{Image: "nginx:1.27-alpine", Port: 80, CPUMillis: 100, MemoryMB: 64, HealthPath: "/"}
	if base.Count() != 1 {
		t.Fatal("unset means one instance")
	}
	s := base
	s.Instances = 3
	if err := s.Validate(); err != nil || s.Count() != 3 {
		t.Fatalf("3 instances: %v", err)
	}
	s.Instances = 11
	if s.Validate() == nil {
		t.Fatal("11 instances accepted")
	}
	s.Instances, s.VolumePath = 2, "/data"
	if s.Validate() == nil {
		t.Fatal("several instances with a persistent folder accepted")
	}
}
