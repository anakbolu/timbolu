package service

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// registry holds every adapter this deployment knows.
//
// Registration happens once, while the container is being assembled, so the lock
// is not for concurrent use: it is there so a read from a task goroutine cannot
// race a late registration during a test.
type registry struct {
	mu       sync.RWMutex
	adapters map[string]domain.Adapter
}

// NewRegistry builds an empty registry.
func NewRegistry() domain.Registry {
	return &registry{adapters: map[string]domain.Adapter{}}
}

// Register adds an adapter.
//
// Every spec it declares is checked here, so an adapter with a typo in a label or
// a name that is not `app.action` fails at startup rather than at the first call
// the model makes.
func (r *registry) Register(adapter domain.Adapter) error {
	if adapter == nil {
		return fmt.Errorf("%w: a nil adapter", domain.ErrInvalidSpec)
	}

	app := strings.TrimSpace(adapter.App())
	if app == "" {
		return fmt.Errorf("%w: an adapter needs an app key", domain.ErrInvalidSpec)
	}

	for _, spec := range adapter.Tools() {
		if err := spec.Valid(); err != nil {
			return err
		}
		// A tool must belong to the app that declares it: `gmail.search` from
		// the jira adapter would be offered under a grant that does not cover it.
		if spec.App() != app {
			return fmt.Errorf("%w: %q declares %q", domain.ErrInvalidSpec, app, spec.Name)
		}
	}
	for _, trigger := range adapter.Triggers() {
		if err := trigger.Valid(); err != nil {
			return err
		}
		if trigger.App != app {
			return fmt.Errorf("%w: %q declares the trigger %q", domain.ErrInvalidSpec, app, trigger.Event)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.adapters[app]; exists {
		return fmt.Errorf("%w: %q", domain.ErrAdapterExists, app)
	}
	r.adapters[app] = adapter
	return nil
}

// Apps lists the registered application keys, sorted.
func (r *registry) Apps() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	apps := make([]string, 0, len(r.adapters))
	for app := range r.adapters {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	return apps
}

// Adapter returns the adapter for an application.
func (r *registry) Adapter(app string) (domain.Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	adapter, ok := r.adapters[app]
	return adapter, ok
}

// Tools lists every tool of every adapter, sorted by name.
func (r *registry) Tools() []domain.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var tools []domain.ToolSpec
	for _, adapter := range r.adapters {
		tools = append(tools, adapter.Tools()...)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// ToolsFor lists one application's tools, sorted by name.
func (r *registry) ToolsFor(app string) []domain.ToolSpec {
	r.mu.RLock()
	adapter, ok := r.adapters[app]
	r.mu.RUnlock()

	if !ok {
		return nil
	}

	tools := adapter.Tools()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// Triggers lists every trigger of every adapter, sorted.
func (r *registry) Triggers() []domain.TriggerSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var triggers []domain.TriggerSpec
	for _, adapter := range r.adapters {
		triggers = append(triggers, adapter.Triggers()...)
	}
	sort.Slice(triggers, func(i, j int) bool { return triggers[i].Event < triggers[j].Event })
	return triggers
}

// Lookup finds a tool by its name.
func (r *registry) Lookup(name string) (domain.ToolSpec, domain.Adapter, bool) {
	app, _, found := strings.Cut(name, ".")
	if !found {
		return domain.ToolSpec{}, nil, false
	}

	r.mu.RLock()
	adapter, ok := r.adapters[app]
	r.mu.RUnlock()
	if !ok {
		return domain.ToolSpec{}, nil, false
	}

	for _, spec := range adapter.Tools() {
		if spec.Name == name {
			return spec, adapter, true
		}
	}
	return domain.ToolSpec{}, nil, false
}

// compile-time check that the registry satisfies its own contract.
var _ domain.Registry = (*registry)(nil)
