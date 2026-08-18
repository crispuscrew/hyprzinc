// Package inherit resolves an app config that starts from another one: a config names a base in
// Inherits and states only what differs. Resolution happens on every read, so editing a base changes
// every app built on it.
//
// The merge is on the YAML rather than on a decoded AppConfig, and that is the design. Decoding first
// loses which keys the child STATED: Go cannot tell `HostTheme: false` from an absent HostTheme. A
// struct merge would have to read every zero value as "inherit", so a child could never turn a base's
// flag off or empty its list - meaning a base granting a capability could not be walked back, and the
// merge would only ever loosen.
//
//   - A key the child states wins, including false and including an empty list.
//   - A key the child omits comes from the base.
//   - Nested blocks merge key by key, so stating one field keeps the base's others.
//   - A stated list replaces the base's entirely: appending would mean a child could never remove an
//     inherited volume or capability.
//
// Resolution is NOT what gets written back. A child is stored as its author wrote it; saving a resolved
// config would flatten the inheritance the first time anyone edited it in the TUI.
package inherit

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxDepth bounds an inheritance chain. Cycles are already caught by name, so this only has
// to stop a pathological but acyclic chain from turning one launch into a thousand file
// reads; nothing legitimate is anywhere near it.
const maxDepth = 8

// baseNameRE is the charset a base name must match. This is a security boundary rather than
// a style rule: the name is joined into a path by whichever store resolves it, so a value
// like "../../etc/evil" would read a config from outside the apps directory. It is the same
// charset AppNameID and DependsOn are held to.
var baseNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Parent reports the base a raw app file inherits from, or "" when it inherits from
// nothing. It decodes only that one field, so a config that is invalid in other ways can
// still have its chain walked - which matters, because a child is usually incomplete on its
// own and only becomes valid once merged.
func Parent(data []byte) (string, error) {
	var head struct {
		Inherits string `yaml:"Inherits"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return "", fmt.Errorf("reading Inherits: %w", err)
	}
	name := strings.TrimSpace(head.Inherits)
	if name == "" {
		return "", nil
	}
	if !baseNameRE.MatchString(name) {
		return "", fmt.Errorf("Inherits %q: only lowercase [a-z0-9._-] allowed, must start alphanumeric", name)
	}
	return name, nil
}

// Resolve walks the chain from the app up through its bases, each base overlaid by the one below, so
// the app has the last word. loadBase is the caller's, since where configs live is the store's
// business. A cycle, a missing base or a chain deeper than maxDepth is an error rather than a partial
// result: what a half-resolved config is missing could be the thing that contains it.
func Resolve(data []byte, loadBase func(name string) ([]byte, error)) ([]byte, error) {
	chain := [][]byte{data}
	seen := map[string]bool{}
	current := data
	for depth := 0; ; depth++ {
		parent, err := Parent(current)
		if err != nil {
			return nil, err
		}
		if parent == "" {
			break
		}
		if seen[parent] {
			return nil, fmt.Errorf("inheritance cycle: %q is already in the chain", parent)
		}
		if depth >= maxDepth {
			return nil, fmt.Errorf("inheritance chain deeper than %d - %q is where it was cut off", maxDepth, parent)
		}
		seen[parent] = true
		baseData, err := loadBase(parent)
		if err != nil {
			return nil, fmt.Errorf("inherits %q: %w", parent, err)
		}
		chain = append(chain, baseData)
		current = baseData
	}
	if len(chain) == 1 {
		return data, nil // inherits from nothing; hand back exactly what came in
	}

	// Fold from the far ancestor down, so each config overlays the one it inherits from and
	// the app being resolved is applied last.
	merged := chain[len(chain)-1]
	for index := len(chain) - 2; index >= 0; index-- {
		next, err := Merge(merged, chain[index])
		if err != nil {
			return nil, err
		}
		merged = next
	}
	return merged, nil
}

// Merge overlays child onto base and returns the result as YAML. Both must be YAML mappings
// (an app config is one); anything else is an error rather than a silent pass-through.
func Merge(base, child []byte) ([]byte, error) {
	baseNode, err := documentRoot(base)
	if err != nil {
		return nil, fmt.Errorf("base: %w", err)
	}
	childNode, err := documentRoot(child)
	if err != nil {
		return nil, fmt.Errorf("child: %w", err)
	}
	switch {
	case baseNode == nil:
		return child, nil
	case childNode == nil:
		return base, nil
	}
	out, err := mergeNode(baseNode, childNode)
	if err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encoding the merged config: %w", err)
	}
	return data, nil
}

// documentRoot decodes YAML down to the node the document holds, or nil for an empty file.
func documentRoot(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, nil // empty file: nothing to merge from or into
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("want a mapping at the top level, got %s", nodeKind(root))
	}
	return root, nil
}

// mergeNode returns base overlaid with child. Two mappings merge key by key and recurse; anything else
// means the child wins, which is what makes a stated list replace an inherited one.
//
// replaceWhole names the mappings whose KEYS are the author's rather than the schema's. Env is the
// case: merging makes it the one field a child cannot narrow, so `Env: {}` left every inherited
// variable in place and a base setting LD_PRELOAD could not be disowned. A stated Env replaces, which
// is what the documented rule already says about a stated list.
var replaceWhole = map[string]bool{"Env": true}

func mergeNode(base, child *yaml.Node) (*yaml.Node, error) {
	if base.Kind != yaml.MappingNode || child.Kind != yaml.MappingNode {
		return child, nil
	}
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: base.Tag, Style: base.Style}
	// Copy the base's pairs, then overwrite the ones the child restates, so the base's key
	// order is preserved and a child's additions land at the end. A config that inherits
	// nothing round-trips through here unchanged in shape.
	for index := 0; index+1 < len(base.Content); index += 2 {
		out.Content = append(out.Content, base.Content[index], base.Content[index+1])
	}
	for index := 0; index+1 < len(child.Content); index += 2 {
		key, value := child.Content[index], child.Content[index+1]
		position := findKey(out, key.Value)
		if position < 0 {
			out.Content = append(out.Content, key, value)
			continue
		}
		if replaceWhole[key.Value] {
			// A child that states this key replaces it outright rather than merging into it.
			out.Content[position+1] = value
			continue
		}
		merged, err := mergeNode(out.Content[position+1], value)
		if err != nil {
			return nil, err
		}
		out.Content[position+1] = merged
	}
	return out, nil
}

// findKey returns the index of a key within a mapping's Content, or -1.
func findKey(mapping *yaml.Node, name string) int {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == name {
			return index
		}
	}
	return -1
}

func nodeKind(node *yaml.Node) string {
	switch node.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.ScalarNode:
		return "a single value"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "something else"
	}
}
