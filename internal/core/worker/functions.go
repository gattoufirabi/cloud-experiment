package worker

import (
	"sync"
	"time"
)

type FnWarm struct {
	ContainerID, Hash string
	WebAddr           string // web apps: the server's address on the project network
	LastUsed          time.Time
	Running           int
}

type FnState struct {
	Mu       sync.Mutex
	Warm     map[string]*FnWarm     // function ID -> warm container
	Draining map[string]*FnWarm     // replaced containers still finishing invocations
	Start    map[string]*sync.Mutex // serializes cold starts per function
	Slots    chan struct{}
}

func (a *Agent) PokeFn() {
	select {
	case a.FnWake <- struct{}{}:
	default:
	}
}
