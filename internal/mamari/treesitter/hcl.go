package treesitter

import (
	"strconv"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

func extractHCL(root *sitter.Node, src []byte) ([]HCLBlock, []HCLAttribute, []HCLTraversal) {
	var blocks []HCLBlock
	var attrs []HCLAttribute
	var refs []HCLTraversal

	var walk func(node *sitter.Node, parentBlockStart, topBlockStart, attrStart int, attrName string)
	walk = func(node *sitter.Node, parentBlockStart, topBlockStart, attrStart int, attrName string) {
		if node == nil {
			return
		}

		switch node.Kind() {
		case "block":
			block := hclBlockFromNode(node, src, parentBlockStart, topBlockStart)
			if block.Type == "" {
				return
			}
			blocks = append(blocks, block)
			cursor := node.Walk()
			defer cursor.Close()
			for _, child := range node.NamedChildren(cursor) {
				if child.Kind() == "body" {
					childCopy := child
					walk(&childCopy, block.Start, block.TopLevelBlockStart, 0, "")
				}
			}
			return
		case "attribute":
			attr := hclAttributeFromNode(node, src, parentBlockStart, topBlockStart)
			if attr.Name == "" {
				return
			}
			attrs = append(attrs, attr)
			cursor := node.Walk()
			defer cursor.Close()
			for _, child := range node.NamedChildren(cursor) {
				if child.Kind() != "identifier" {
					childCopy := child
					walk(&childCopy, parentBlockStart, topBlockStart, attr.Start, attr.Name)
				}
			}
			return
		case "expression":
			if !hclExpressionIsObjectKey(node) {
				refs = append(refs, hclTraversalsFromExpression(node, src, topBlockStart, attrStart, attrName)...)
			}
		}

		cursor := node.Walk()
		defer cursor.Close()
		for _, child := range node.NamedChildren(cursor) {
			childCopy := child
			walk(&childCopy, parentBlockStart, topBlockStart, attrStart, attrName)
		}
	}

	walk(root, 0, 0, 0, "")
	return blocks, attrs, refs
}

func hclBlockFromNode(node *sitter.Node, src []byte, parentBlockStart, topBlockStart int) HCLBlock {
	cursor := node.Walk()
	defer cursor.Close()
	children := node.NamedChildren(cursor)
	if len(children) == 0 || children[0].Kind() != "identifier" {
		return HCLBlock{}
	}

	block := HCLBlock{
		Type:             children[0].Utf8Text(src),
		Start:            int(node.StartByte()),
		End:              int(node.EndByte()),
		ParentBlockStart: parentBlockStart,
		TopLevel:         topBlockStart == 0,
	}
	if block.TopLevel {
		block.TopLevelBlockStart = block.Start
	} else {
		block.TopLevelBlockStart = topBlockStart
	}
	for _, child := range children[1:] {
		switch child.Kind() {
		case "string_lit":
			if label, ok := hclStaticString(&child, src); ok {
				block.Labels = append(block.Labels, label)
			}
		case "block_start":
			block.HeaderEnd = int(child.EndByte())
			return block
		}
	}
	return block
}

func hclAttributeFromNode(node *sitter.Node, src []byte, blockStart, topBlockStart int) HCLAttribute {
	cursor := node.Walk()
	defer cursor.Close()
	children := node.NamedChildren(cursor)
	if len(children) < 2 || children[0].Kind() != "identifier" {
		return HCLAttribute{}
	}
	attr := HCLAttribute{
		Name:               children[0].Utf8Text(src),
		Start:              int(node.StartByte()),
		End:                int(node.EndByte()),
		NameStart:          int(children[0].StartByte()),
		NameEnd:            int(children[0].EndByte()),
		BlockStart:         blockStart,
		TopLevelBlockStart: topBlockStart,
	}
	if value, ok := hclStaticString(&children[1], src); ok {
		attr.StaticValue = value
	}
	return attr
}

func hclStaticString(node *sitter.Node, src []byte) (string, bool) {
	if node == nil {
		return "", false
	}
	raw := strings.TrimSpace(node.Utf8Text(src))
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' || strings.Contains(raw, "${") || strings.Contains(raw, "%{") {
		return "", false
	}
	value, err := strconv.Unquote(raw)
	if err != nil {
		return strings.Trim(raw, `"`), true
	}
	return value, true
}

func hclExpressionIsObjectKey(node *sitter.Node) bool {
	parent := node.Parent()
	if parent == nil || parent.Kind() != "object_elem" {
		return false
	}
	key := parent.ChildByFieldName("key")
	return key != nil && key.StartByte() == node.StartByte() && key.EndByte() == node.EndByte()
}

func hclTraversalsFromExpression(node *sitter.Node, src []byte, topBlockStart, attrStart int, attrName string) []HCLTraversal {
	cursor := node.Walk()
	defer cursor.Close()
	children := node.NamedChildren(cursor)
	var refs []HCLTraversal
	for i := 0; i < len(children); i++ {
		if children[i].Kind() != "variable_expr" {
			continue
		}
		rootCursor := children[i].Walk()
		rootChildren := children[i].NamedChildren(rootCursor)
		rootCursor.Close()
		if len(rootChildren) != 1 || rootChildren[0].Kind() != "identifier" {
			continue
		}
		parts := []string{rootChildren[0].Utf8Text(src)}
		end := int(children[i].EndByte())
		for j := i + 1; j < len(children); j++ {
			switch children[j].Kind() {
			case "get_attr":
				attrCursor := children[j].Walk()
				attrChildren := children[j].NamedChildren(attrCursor)
				attrCursor.Close()
				if len(attrChildren) == 1 && attrChildren[0].Kind() == "identifier" {
					parts = append(parts, attrChildren[0].Utf8Text(src))
					end = int(children[j].EndByte())
				}
			case "index", "splat":
				end = int(children[j].EndByte())
			default:
				j = len(children)
			}
		}
		refs = append(refs, HCLTraversal{
			Parts:              parts,
			Start:              int(children[i].StartByte()),
			End:                end,
			AttributeStart:     attrStart,
			AttributeName:      attrName,
			TopLevelBlockStart: topBlockStart,
		})
	}
	return refs
}
