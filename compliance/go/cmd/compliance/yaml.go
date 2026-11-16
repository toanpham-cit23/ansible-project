package main

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// This file isolates the yaml.v3 dependency behind a small node-level API.
//
// Decoding into map[string]any would lose two things the validation logic
// depends on: the original document order of the routers, and whether a mapping
// key was a string or a number. yaml.Node preserves both, which is what lets
// loadDesiredState reject a numeric router key the same way PyYAML does.

// Node kinds, re-exported so main.go does not need to import yaml.
const (
	documentNode = yaml.DocumentNode
	sequenceNode = yaml.SequenceNode
	mappingNode  = yaml.MappingNode
	scalarNode   = yaml.ScalarNode
	aliasNode    = yaml.AliasNode
)

// yamlNode is an alias so the rest of the package can hold a node without
// importing yaml directly.
type yamlNode = yaml.Node

func unmarshalYAML(data []byte, out *yamlNode) error {
	return yaml.Unmarshal(data, out)
}

// documentField returns the value node for key at the top level of the document.
//
// A missing key returns (nil, nil) rather than an error so the caller can apply
// its own message, which is what the Python code does when it checks
// isinstance(data.get("routers"), dict).
func documentField(doc *yaml.Node, key string) (*yaml.Node, error) {
	if doc == nil {
		return nil, fmt.Errorf("tài liệu YAML rỗng")
	}
	root := doc
	if root.Kind == documentNode {
		if len(root.Content) == 0 {
			return nil, fmt.Errorf("tài liệu YAML rỗng")
		}
		root = root.Content[0]
	}
	if root.Kind != mappingNode {
		return nil, fmt.Errorf("phải là mapping ở cấp cao nhất")
	}
	return mappingField(root, key)
}

// mappingField returns the value node for key in a mapping node, or (nil, nil)
// when the key is absent.
func mappingField(m *yaml.Node, key string) (*yaml.Node, error) {
	if m == nil || m.Kind != mappingNode {
		return nil, fmt.Errorf("phải là mapping")
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Kind == scalarNode && k.Value == key {
			// Follow aliases so `routers: *anchor` behaves like the literal.
			for v.Kind == aliasNode && v.Alias != nil {
				v = v.Alias
			}
			return v, nil
		}
	}
	return nil, nil
}

// errorsAs and errorsIs wrap stdlib helpers so main.go stays readable.
func errorsAs(err error, target any) bool { return errors.As(err, target) }
