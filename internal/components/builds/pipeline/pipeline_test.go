package pipeline

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	p, err := Parse([]byte(`
steps:
  - name: unit tests
    image: golang:1.23
    run: go test ./...
    env: {CGO_ENABLED: "0"}
    timeout: 5m
  - build
  - name: smoke
    image: built
    run: /app/server --selftest
    secrets: [DATABASE_URL, API_KEY]
deploy: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 3 || !p.Steps[1].Build || p.Steps[0].Timeout != 5*time.Minute || p.Steps[2].Timeout != defaultTimeout || p.Deploy {
		t.Fatalf("parsed: %+v", p)
	}
	if got := strings.Join(p.SecretNames(), ","); got != "DATABASE_URL,API_KEY" {
		t.Fatalf("secrets: %s", got)
	}

	// Without a build step, the image is built last; deploy defaults to true.
	p, err = Parse([]byte("steps:\n  - name: lint\n    image: node:22\n    run: npm run lint\n"))
	if err != nil || len(p.Steps) != 2 || !p.Steps[1].Build || !p.Deploy {
		t.Fatalf("default build: %+v %v", p, err)
	}
}

func TestParseErrors(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "at least one step",
		"steps: []":                    "at least one step",
		"stages: [a]":                  "field stages not found",
		"steps:\n  - deploy":           `"deploy" is not a step`,
		"steps:\n  - build\n  - build": "only once",
		"steps:\n  - name: a\n    image: built\n    run: x\n  - build":                               "needs the build step before it",
		"steps:\n  - name: a\n    image: node:22":                                                    "set run",
		"steps:\n  - name: a\n    run: x":                                                            "set image",
		"steps:\n  - name: a\n    image: 'Bad Image'\n    run: x":                                    "not an image name",
		"steps:\n  - name: a\n    image: node\n    comand: x":                                        "comand",
		"steps:\n  - name: a\n    image: node\n    run: x\n    timeout: 2h":                          "timeout",
		"steps:\n  - name: a\n    image: node\n    run: x\n    secrets: [lower]":                     "secret",
		"steps:\n  - name: a\n    image: node\n    run: x\n  - name: a\n    image: node\n    run: y": "already named",
	} {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error containing %q", in, err, want)
		}
	}
}

func TestHider(t *testing.T) {
	h := hider(map[string]string{"A": "supersecret", "B": "abc"}, []string{"A", "B"})
	if got := h("token supersecret and abc"); got != "token *** and abc" {
		t.Fatal(got)
	}
}
