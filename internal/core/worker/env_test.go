package worker

import (
	"reflect"
	"testing"
)

func TestDedupEnvLastWins(t *testing.T) {
	got := DedupEnv([]string{"A=project", "B=1", "A=service", "C=x=y", "A=secret"})
	want := []string{"A=secret", "B=1", "C=x=y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
