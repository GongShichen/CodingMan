package tool

import "testing"

func TestRegistryCloneCopiesToolsWithoutSharingMaps(t *testing.T) {
	registry := NewRegistry()
	readTool := NewReadTool()
	if err := registry.Register(readTool); err != nil {
		t.Fatal(err)
	}

	clone := registry.Clone()
	if clone == nil {
		t.Fatal("clone is nil")
	}
	if _, err := clone.Get(readTool.Name()); err != nil {
		t.Fatalf("cloned registry missing tool: %v", err)
	}

	clone.Unregister(readTool.Name())
	if _, err := registry.Get(readTool.Name()); err != nil {
		t.Fatalf("original registry should keep tool after clone mutation: %v", err)
	}
}
