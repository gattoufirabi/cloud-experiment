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

func TestPublicPorts(t *testing.T) {
	ports, err := ParsePorts("25565/tcp\n27015/udp, 8080 25565/TCP")
	if err != nil || len(ports) != 3 || ports[2].Key() != "8080/tcp" {
		t.Fatalf("parse: %v %v", ports, err)
	}
	for _, bad := range []string{"70000", "25565/sctp", "abc", "1,2,3,4,5,6"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	s := Spec{Image: "nginx:1", Worker: true, CPUMillis: 100, MemoryMB: 64, Ports: ports}
	if err := s.Validate(); err != nil {
		t.Errorf("valid spec refused: %v", err)
	}
	s.Instances = 2
	if err := s.Validate(); err == nil {
		t.Error("public ports with two instances accepted")
	}
}
