package workflow

import "fmt"

// Registry maps action names (e.g. "vscode.create_session") to handler functions.
type Registry struct {
	actions map[string]ActionFunc
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{actions: make(map[string]ActionFunc)}
}

// Register adds an action handler. Panics on duplicate names to catch
// programming errors early.
func (r *Registry) Register(name string, fn ActionFunc) {
	if _, exists := r.actions[name]; exists {
		panic(fmt.Sprintf("workflow: duplicate action registered: %s", name))
	}
	r.actions[name] = fn
}

// Get returns the handler for the given action name, or nil if not found.
func (r *Registry) Get(name string) ActionFunc {
	return r.actions[name]
}

// DefaultRegistry returns a Registry pre-loaded with all built-in actions.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	registerBuiltinActions(r)
	return r
}
