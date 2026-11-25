package buildsrc

import (
	"os"
	"strings"
	"testing"
)

func TestBuilderDockerfileMatchesDeploy(t *testing.T) {
	b, err := os.ReadFile("../../../../deploy/builder/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(b), "\r\n", "\n") != BuilderDockerfile {
		t.Fatal("BuilderDockerfile differs from deploy/builder/Dockerfile: copy it over")
	}
}
