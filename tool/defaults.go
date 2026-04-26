package tool

func NewDefaultRegistry() *Registry {
	registry := NewRegistry()

	_ = registry.Register(NewBashTool())
	_ = registry.Register(NewReadTool())
	_ = registry.Register(NewWriteTool())
	_ = registry.Register(NewEditTool())
	_ = registry.Register(NewGlobTool())
	_ = registry.Register(NewGrepTool())
	return registry
}
