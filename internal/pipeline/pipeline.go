// Package pipeline runs build pipelines: an optional staysa.yml in the folder a service
// builds lists steps (tests, scripts, migrations) run in containers around the image
// build, on the platform's builder and on build servers alike. Every step shares the
// checked-out source (the workspace); "build" marks where the image is built; a step
// with image "built" runs in that image. The image is pushed and deployed only when
// every step passed.
//
//	steps:
//	  - name: unit tests
//	    image: golang:1.23
//	    run: go test ./...
//	  - build
//	  - name: smoke test
//	    image: built
//	    run: /app/server --selftest
//	    secrets: [DATABASE_URL]
//	deploy: true
package pipeline

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the pipeline file, in the folder the service builds.
const FileName = "staysa.yml"

// MaxFileBytes caps the pipeline file.
const MaxFileBytes = 64 << 10

const (
	maxSteps       = 20
	defaultTimeout = 10 * time.Minute
	// BuiltImage is the image name of a step that runs in the image just built.
	BuiltImage = "built"
)

// Step is one step: the image build, or a command in a container.
type Step struct {
	Name    string
	Build   bool // the image build
	Image   string
	Run     string
	Env     map[string]string
	Secrets []string
	Timeout time.Duration
}

// Pipeline is a parsed staysa.yml.
type Pipeline struct {
	Steps  []Step
	Deploy bool // deploy after a successful build (when the build asked to)
}

// SecretNames lists the secrets the steps ask for, once each.
func (p *Pipeline) SecretNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range p.Steps {
		for _, n := range s.Secrets {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

type rawStep struct {
	Name    string            `yaml:"name"`
	Image   string            `yaml:"image"`
	Run     string            `yaml:"run"`
	Env     map[string]string `yaml:"env"`
	Secrets []string          `yaml:"secrets"`
	Timeout string            `yaml:"timeout"`
}

var (
	envRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	secRe   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	imageRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]{0,199}$`)
)

// Parse reads and checks a pipeline file. The error says what to fix, with the step.
func Parse(data []byte) (*Pipeline, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%s is larger than %d KB", FileName, MaxFileBytes>>10)
	}
	var doc struct {
		Steps  []yaml.Node `yaml:"steps"`
		Deploy *bool       `yaml:"deploy"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %s", FileName, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if len(doc.Steps) == 0 {
		return nil, fmt.Errorf("%s: list at least one step under steps", FileName)
	}
	if len(doc.Steps) > maxSteps {
		return nil, fmt.Errorf("%s: at most %d steps", FileName, maxSteps)
	}
	p := &Pipeline{Deploy: doc.Deploy == nil || *doc.Deploy}
	builds, built := 0, false
	names := map[string]bool{}
	for i, n := range doc.Steps {
		where := fmt.Sprintf("%s: step %d", FileName, i+1)
		if n.Kind == yaml.ScalarNode {
			if n.Value != "build" {
				return nil, fmt.Errorf("%s: %q is not a step; write build, or a step with name, image and run", where, n.Value)
			}
			builds++
			built = true
			p.Steps = append(p.Steps, Step{Name: "build", Build: true})
			continue
		}
		var r rawStep
		if err := n.Decode(&r); err != nil {
			return nil, fmt.Errorf("%s: %s", where, strings.TrimPrefix(err.Error(), "yaml: "))
		}
		if err := checkKeys(&n); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		s := Step{Name: strings.TrimSpace(r.Name), Image: strings.TrimSpace(r.Image), Run: r.Run, Env: r.Env, Secrets: r.Secrets, Timeout: defaultTimeout}
		switch {
		case s.Name == "" || len(s.Name) > 60 || strings.ContainsAny(s.Name, "\r\n"):
			return nil, fmt.Errorf("%s: give it a name (up to 60 characters)", where)
		case names[s.Name]:
			return nil, fmt.Errorf("%s: another step is already named %q", where, s.Name)
		case s.Image == "":
			return nil, fmt.Errorf("%s (%s): set image, such as node:22 or %s for the image just built", where, s.Name, BuiltImage)
		case s.Image != BuiltImage && !imageRe.MatchString(s.Image):
			return nil, fmt.Errorf("%s (%s): %q is not an image name", where, s.Name, s.Image)
		case s.Image == BuiltImage && !built:
			return nil, fmt.Errorf("%s (%s): image %s needs the build step before it", where, s.Name, BuiltImage)
		case strings.TrimSpace(s.Run) == "":
			return nil, fmt.Errorf("%s (%s): set run, the commands to run", where, s.Name)
		case len(s.Run) > 16<<10:
			return nil, fmt.Errorf("%s (%s): run is longer than 16 KB", where, s.Name)
		case len(s.Env) > 50:
			return nil, fmt.Errorf("%s (%s): at most 50 env variables", where, s.Name)
		case len(s.Secrets) > 20:
			return nil, fmt.Errorf("%s (%s): at most 20 secrets", where, s.Name)
		}
		for k, v := range s.Env {
			if !envRe.MatchString(k) || len(v) > 4096 {
				return nil, fmt.Errorf("%s (%s): env %q: letters, digits and underscores, values up to 4 KB", where, s.Name, k)
			}
		}
		for _, sec := range s.Secrets {
			if !secRe.MatchString(sec) {
				return nil, fmt.Errorf("%s (%s): secret %q: the name of one of the service's secrets, such as DATABASE_URL", where, s.Name, sec)
			}
		}
		if r.Timeout != "" {
			d, err := time.ParseDuration(r.Timeout)
			if err != nil || d < 10*time.Second || d > time.Hour {
				return nil, fmt.Errorf("%s (%s): timeout: a duration from 10s to 1h, such as 5m", where, s.Name)
			}
			s.Timeout = d
		}
		names[s.Name] = true
		p.Steps = append(p.Steps, s)
	}
	switch {
	case builds > 1:
		return nil, errors.New(FileName + ": build may appear only once")
	case builds == 0:
		p.Steps = append(p.Steps, Step{Name: "build", Build: true}) // the image is built last
	}
	return p, nil
}

// checkKeys refuses unknown keys in a step (a typo like "comand" would be ignored).
func checkKeys(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("write build, or a step with name, image and run")
	}
	for i := 0; i < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "name", "image", "run", "env", "secrets", "timeout":
		default:
			return fmt.Errorf("unknown key %q (keys: name, image, run, env, secrets, timeout)", k)
		}
	}
	return nil
}
