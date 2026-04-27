package tool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	ErrToolNotFound          = errors.New("tool not found")
	ErrToolAlreadyRegistered = errors.New("tool already registered")
	ErrNilTool               = errors.New("tool is nil")
	ErrNilToolFactory        = errors.New("tool factory is nil")
)

type Factory func() Tool

type Registry struct {
	mu        sync.RWMutex
	tools     map[string]Tool
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{
		tools:     make(map[string]Tool),
		factories: make(map[string]Factory),
	}
}

func (r *Registry) Clone() *Registry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	clone := NewRegistry()
	for name, tool := range r.tools {
		clone.tools[name] = tool
	}
	for name, factory := range r.factories {
		clone.factories[name] = factory
	}
	return clone
}

func (r *Registry) Register(tool Tool) error {
	if tool == nil {
		return ErrNilTool
	}

	name := tool.Name()
	if name == "" {
		return errors.New("tool name is empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("%w: %s", ErrToolAlreadyRegistered, name)
	}
	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("%w: %s", ErrToolAlreadyRegistered, name)
	}

	r.tools[name] = tool
	return nil
}

func (r *Registry) RegisterFactory(name string, factory Factory) error {
	if factory == nil {
		return ErrNilToolFactory
	}
	if name == "" {
		return errors.New("tool name is empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("%w: %s", ErrToolAlreadyRegistered, name)
	}
	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("%w: %s", ErrToolAlreadyRegistered, name)
	}

	r.factories[name] = factory
	return nil
}

func (r *Registry) Get(name string) (Tool, error) {
	r.mu.RLock()
	tool, toolExists := r.tools[name]
	factory, factoryExists := r.factories[name]
	r.mu.RUnlock()

	if toolExists {
		return tool, nil
	}
	if factoryExists {
		created := factory()
		if created == nil {
			return nil, fmt.Errorf("tool factory returned nil: %s", name)
		}
		return created, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrToolNotFound, name)
}

func (r *Registry) MustGet(name string) Tool {
	tool, err := r.Get(name)
	if err != nil {
		panic(err)
	}
	return tool
}

func (r *Registry) Call(name string, input map[string]any) (string, error) {
	return r.CallContext(context.Background(), name, input)
}

func (r *Registry) CallContext(ctx context.Context, name string, input map[string]any) (string, error) {
	tool, err := r.Get(name)
	if err != nil {
		return "", err
	}
	if contextTool, ok := tool.(ContextTool); ok {
		return contextTool.CallContext(ctx, input)
	}
	return tool.Call(input)
}

func (r *Registry) Tools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Tool, 0, len(r.tools)+len(r.factories))

	for _, tool := range r.tools {
		result = append(result, tool)
	}

	for name, factory := range r.factories {
		tool := factory()
		if tool == nil {
			continue
		}
		if tool.Name() == "" {
			tool = namedFactoryTool{name: name, tool: tool}
		}
		result = append(result, tool)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name() < result[j].Name()
	})

	return result
}

func (r *Registry) List() []ToolSchema {
	tools := r.Tools()
	schemas := make([]ToolSchema, 0, len(tools))

	for _, tool := range tools {
		schemas = append(schemas, ToolSchema{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: tool.InputSchema(),
		})
	}

	return schemas
}

func (r *Registry) ToAPIFormat() []map[string]any {
	schemas := r.List()
	result := make([]map[string]any, 0, len(schemas))
	for _, schema := range schemas {
		result = append(result, map[string]any{
			"name":         schema.Name,
			"description":  schema.Description,
			"input_schema": schema.InputSchema,
		})
	}
	return result
}

func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
	delete(r.factories, name)
}

func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = make(map[string]Tool)
	r.factories = make(map[string]Factory)
}

type namedFactoryTool struct {
	name string
	tool Tool
}

func (n namedFactoryTool) Name() string                { return n.name }
func (n namedFactoryTool) Description() string         { return n.tool.Description() }
func (n namedFactoryTool) InputSchema() map[string]any { return n.tool.InputSchema() }
func (n namedFactoryTool) Call(input map[string]any) (string, error) {
	return n.tool.Call(input)
}
func (n namedFactoryTool) CallContext(ctx context.Context, input map[string]any) (string, error) {
	if contextTool, ok := n.tool.(ContextTool); ok {
		return contextTool.CallContext(ctx, input)
	}
	return n.tool.Call(input)
}
func (n namedFactoryTool) ToAPIFormat() map[string]any {
	return map[string]any{
		"name":         n.Name(),
		"description":  n.Description(),
		"input_schema": n.InputSchema(),
	}
}
