package toolUse

type Tool interface {
	Name() string
	Description() string
	InputSchema() map[string]any
	Call(input map[string]any) (string, error)
	ToAPIFormat() map[string]any
}

type ToolSchema struct {
	Name        string
	Description string
	InputSchema map[string]any
}

type BaseTool struct {
	name        string
	description string
	inputSchema map[string]any
}

func NewBaseTool(name string, description string, inputSchema map[string]any) BaseTool {
	return BaseTool{
		name:        name,
		description: description,
		inputSchema: inputSchema,
	}
}

func (tool BaseTool) Name() string        { return tool.name }
func (tool BaseTool) Description() string { return tool.description }
func (tool BaseTool) InputSchema() map[string]any {
	return tool.inputSchema
}

func (tool BaseTool) Call(input map[string]any) (string, error) {
	_ = input
	return "", nil
}

func (tool BaseTool) ToAPIFormat() map[string]any {
	return map[string]any{
		"name":         tool.Name(),
		"description":  tool.Description(),
		"input_schema": tool.InputSchema(),
	}
}
