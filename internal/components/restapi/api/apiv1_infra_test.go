package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"platform/internal/core/web"
	"platform/internal/lib/model"
)

// A partial change keeps every setting it does not name; named ones replace them.
func TestServiceJSONMergesWithCurrentSettings(t *testing.T) {
	sv := &web.ServiceInfo{Image: "nginx:1", Port: 80, CPUMillis: 250, MemoryMB: 256, HealthPath: "/", Instances: 2,
		Env: map[string]string{"A": "1"}, AutoCPU: 70, Public: true, MetricsPath: "/metrics",
		Ports: []model.PublicPort{{ContainerPort: 25565, Protocol: "tcp"}}}
	f := serviceForm(sv)
	deploy, err := applyServiceJSON(jsonFields(t, `{"image": "nginx:2", "env": {"B": "x=y"}, "public": false, "deploy": true}`), f)
	if err != nil {
		t.Fatal(err)
	}
	if !deploy || f.Get("image") != "nginx:2" || f.Get("port") != "80" || f.Get("instances") != "2" || f.Get("metrics_path") != "/metrics" {
		t.Errorf("merge lost or ignored a field: %v", f)
	}
	if env, _ := model.ParseEnv(f.Get("env")); len(env) != 1 || env["B"] != "x=y" {
		t.Errorf("env not replaced: %q", f.Get("env"))
	}
	if f.Get("private") != "1" || f.Get("ports") != "25565/tcp" {
		t.Errorf("public flag or ports wrong: %v", f)
	}
}

func TestServiceJSONAutoscaling(t *testing.T) {
	f := serviceForm(&web.ServiceInfo{Image: "x", Port: 80, CPUMillis: 250, MemoryMB: 256, AutoCPU: 70, Public: true})
	if _, err := applyServiceJSON(jsonFields(t, `{"autoscaling": {"min": 1, "max": 4}}`), f); err != nil {
		t.Fatal(err)
	}
	if f.Get("autoscale") != "1" || f.Get("autoscale_min") != "1" || f.Get("autoscale_max") != "4" || f.Get("autoscale_cpu") != "70" {
		t.Errorf("autoscaling not set: %v", f)
	}
	if _, err := applyServiceJSON(jsonFields(t, `{"autoscaling": null}`), f); err != nil || f.Get("autoscale") != "" {
		t.Errorf("autoscaling not turned off: %v %v", err, f)
	}
}

func TestServiceJSONRefusesBadInput(t *testing.T) {
	for _, body := range []string{`{"env": {"A": "two\nlines"}}`, `{"env": {"A=B": "1"}}`, `{"port": "eighty"}`, `{"autoscaling": 3}`} {
		if _, err := applyServiceJSON(jsonFields(t, body), serviceForm(&web.ServiceInfo{})); err == nil {
			t.Errorf("%s accepted", body)
		}
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"imgae": "x"}`))
	if _, err := decodeObject(r, serviceFields...); err == nil || !strings.Contains(err.Error(), "imgae") {
		t.Errorf("unknown field not refused: %v", err)
	}
}

func jsonFields(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var in map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	return in
}
