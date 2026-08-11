// Package inherit resolves an app config that starts from another one. A config names a
// base in Inherits and states only what differs; what it does not state, it takes from the
// base. Resolution happens every time a config is read, so editing a base changes every app
// built on it - which is the point, and also the thing to be careful with.
//
// The merge is performed on the YAML itself rather than on a decoded AppConfig, and that is
// the whole design. Decoding first would lose the one fact the merge depends on: which keys
// the child actually STATED. Go cannot tell `HostTheme: false` from an absent HostTheme, nor
// an empty Volumes list from an omitted one - both arrive as the zero value. Merging decoded
// structs would therefore have to read every zero value as "inherit", which means a child
// could never turn a base's flag off and never empty a base's list. On a sandboxing tool
// those are not cosmetic gaps: a base setting DisableSecurityContext or granting a
// capability would be impossible to walk back in a child, and the merge would only ever
// loosen. Merging nodes has no such rule, because a stated key is visibly present.
//
// The semantics that fall out are the simple ones:
//
//   - A key the child states wins, whatever its value - including false, and including an
//     empty list.
//   - A key the child omits is taken from the base.
//   - Nested blocks merge key by key, so a child stating one field of StartConditions keeps
//     the base's other fields rather than replacing the block.
//   - A list the child states replaces the base's entirely. Lists are not merged element by
//     element: appending would mean a child could never remove an inherited volume or
//     capability, and capabilities that only ever accumulate down a chain are the wrong
//     direction for this tool.
//
// Resolution is deliberately NOT what gets written back. A child is stored as its author
// wrote it; only the reading side merges. Saving a resolved config would flatten the
// inheritance the first time anyone edited an app in the TUI.
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

// Resolve returns the app config data with its inheritance applied: the chain is walked from
// the app up through its bases, and each base is overlaid by the one below it, so the app
// itself has the last word.
//
// loadBase reads a base's raw bytes by name; it is the caller's, because where configs live
// is the store's business and this package performs no I/O. A cycle, a missing base, or a
// chain deeper than maxDepth is an error rather than a partial result - a config that cannot
// be fully resolved must not be run, since what it is missing could be the thing that
// contains it.
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

// mergeNode returns base overlaid with child. Two mappings merge key by key and recurse;
// anything else means the child simply wins, which is what makes a stated list replace an
// inherited one and a stated scalar - false included - override.
// replaceWhole names the mappings whose KEYS are the author's rather than the schema's.
//
// The rule for a nested block is that it merges field by field, and that is right: those keys
// are a fixed set the schema defines, so a child restating one field should not silently drop
// the base's others. Env is not that. Its keys are chosen by whoever wrote the config, so
// merging makes it the one field a child cannot narrow: `Env: {}` left every inherited
// variable in place, and a base setting LD_PRELOAD or SSL_CERT_FILE could not be disowned by
// any child, while the child's own file read as though it had none. That is the loosening a
// base cannot be walked back from, which the package comment gives as the reason for merging
// nodes rather than structs in the first place.
//
// So a stated Env replaces, which is also what the documented rule already says about a
// stated list.
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
