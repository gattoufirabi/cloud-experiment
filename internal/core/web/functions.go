package web

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"platform/internal/lib/functions"
)

type FunctionInfo struct {
	ID, Slug, Runtime, Handler, CodeSHA, URLAuth, Schedule string
	WebCommand                                             string // web apps: the server's start command ("" = handler)
	WebPort                                                int
	Code                                                   []byte
	MemoryMB, TimeoutSeconds, Concurrency                  int
	Env                                                    map[string]string
	URLEnabled, ScheduleEnabled, HasToken                  bool
	NextRun                                                *time.Time
	Created, Updated                                       time.Time
}

func (f *FunctionInfo) RuntimeName() string {
	if rt, ok := functions.FindRuntime(f.Runtime); ok {
		return rt.Name
	}
	return f.Runtime
}

// EnvText is the variables as KEY=VALUE lines, for the settings form.
func (f *FunctionInfo) EnvText() string {
	keys := make([]string, 0, len(f.Env))
	for k := range f.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + f.Env[k] + "\n")
	}
	return b.String()
}

// fnWaiters wakes requests waiting for an invocation when the agent notifies fn_done.
type fnWaiters struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (fw *fnWaiters) Add(id string) chan struct{} {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.m == nil {
		fw.m = map[string]chan struct{}{}
	}
	ch := make(chan struct{}, 1)
	fw.m[id] = ch
	return ch
}

func (fw *fnWaiters) Done(id string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if ch, ok := fw.m[id]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (fw *fnWaiters) Remove(id string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	delete(fw.m, id)
}

func runtimeLabel(id string) string {
	if rt, ok := functions.FindRuntime(id); ok {
		return rt.Name
	}
	return id
}

func itoaPtr(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}
