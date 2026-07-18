package mamari

import (
	"path"
	"sort"
	"strings"

	"github.com/waelhoury/mamari/internal/mamari/treesitter"
)

const terraformDependencyEdge = "depends-on"

func isTerraformSymbolKind(kind string) bool {
	return strings.HasPrefix(kind, "terraform-")
}

type terraformSymbolSpec struct {
	name     string
	kind     string
	exported bool
}

func isTerraformNativeConfigFile(file string) bool {
	lower := strings.ToLower(path.Base(filepathToSlash(file)))
	return strings.HasSuffix(lower, ".tf") || strings.HasSuffix(lower, ".tftest.hcl")
}

func filepathToSlash(file string) string {
	return strings.ReplaceAll(file, "\\", "/")
}

func emitTerraformSymbolsTS(idx *Index, file, content, parentID string) {
	res, err := treesitter.Parse("hcl", []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	if !res.ParseOK {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "parse_error", Message: res.ParseError}})
	}

	starts := lineStarts(content)
	lines := strings.Split(content, "\n")
	for _, block := range res.HCLBlocks {
		if !block.TopLevel || block.Type == "locals" {
			continue
		}
		spec := terraformBlockSymbolSpec(block, res.HCLAttrs)
		if spec.name == "" || spec.kind == "" {
			continue
		}
		startLine, startCol := offsetToLineCol(starts, block.Start)
		endLine, endCol := offsetToLineCol(starts, clampOffset(content, block.End))
		doc := terraformDirectStaticAttribute(res.HCLAttrs, block.Start, "description")
		if doc == "" {
			doc = extractSymbolDocstring(lines, startLine, "hcl")
		}
		idx.AddCGPSymbol(CGPSymbol{
			ID:          stableSymbolID("hcl", spec.kind, file, spec.name, idx),
			Name:        spec.name,
			Kind:        spec.kind,
			Language:    "hcl",
			File:        file,
			StartLine:   startLine,
			StartColumn: startCol,
			EndLine:     endLine,
			EndColumn:   endCol,
			Signature:   terraformBlockSignature(content, block),
			Docstring:   doc,
			Exported:    spec.exported,
			ParentID:    parentID,
			Confidence:  ConfExact,
		})
	}

	for _, attr := range res.HCLAttrs {
		block, ok := terraformTopLevelBlockByStart(res.HCLBlocks, attr.TopLevelBlockStart)
		if !ok || block.Type != "locals" || attr.BlockStart != block.Start {
			continue
		}
		name := "local." + attr.Name
		startLine, startCol := offsetToLineCol(starts, attr.Start)
		endLine, endCol := offsetToLineCol(starts, clampOffset(content, attr.End))
		idx.AddCGPSymbol(CGPSymbol{
			ID:          stableSymbolID("hcl", "terraform-local", file, name, idx),
			Name:        name,
			Kind:        "terraform-local",
			Language:    "hcl",
			File:        file,
			StartLine:   startLine,
			StartColumn: startCol,
			EndLine:     endLine,
			EndColumn:   endCol,
			Signature:   terraformAttributeSignature(content, attr),
			Docstring:   extractSymbolDocstring(lines, startLine, "hcl"),
			ParentID:    parentID,
			Confidence:  ConfExact,
		})
	}
}

func terraformBlockSymbolSpec(block treesitter.HCLBlock, attrs []treesitter.HCLAttribute) terraformSymbolSpec {
	labels := block.Labels
	switch block.Type {
	case "resource":
		if len(labels) >= 2 {
			return terraformSymbolSpec{name: labels[0] + "." + labels[1], kind: "terraform-resource"}
		}
	case "data":
		if len(labels) >= 2 {
			return terraformSymbolSpec{name: "data." + labels[0] + "." + labels[1], kind: "terraform-data"}
		}
	case "ephemeral":
		if len(labels) >= 2 {
			return terraformSymbolSpec{name: "ephemeral." + labels[0] + "." + labels[1], kind: "terraform-ephemeral"}
		}
	case "action":
		if len(labels) >= 2 {
			return terraformSymbolSpec{name: "action." + labels[0] + "." + labels[1], kind: "terraform-action"}
		}
	case "variable":
		if len(labels) >= 1 {
			return terraformSymbolSpec{name: "var." + labels[0], kind: "terraform-variable", exported: true}
		}
	case "module":
		if len(labels) >= 1 {
			return terraformSymbolSpec{name: "module." + labels[0], kind: "terraform-module", exported: true}
		}
	case "output":
		if len(labels) >= 1 {
			return terraformSymbolSpec{name: "output." + labels[0], kind: "terraform-output", exported: true}
		}
	case "provider":
		if len(labels) >= 1 {
			name := "provider." + labels[0]
			if alias := terraformDirectStaticAttribute(attrs, block.Start, "alias"); alias != "" {
				name += "." + alias
			}
			return terraformSymbolSpec{name: name, kind: "terraform-provider"}
		}
	case "terraform":
		return terraformSymbolSpec{name: "terraform", kind: "terraform-config"}
	case "check":
		if len(labels) >= 1 {
			return terraformSymbolSpec{name: "check." + labels[0], kind: "terraform-check"}
		}
	case "run":
		if len(labels) >= 1 {
			return terraformSymbolSpec{name: "run." + labels[0], kind: "terraform-test-run"}
		}
	case "mock_provider":
		if len(labels) >= 1 {
			return terraformSymbolSpec{name: "mock_provider." + labels[0], kind: "terraform-mock-provider"}
		}
	}

	if len(labels) > 0 {
		return terraformSymbolSpec{
			name: block.Type + "." + strings.Join(labels, "."),
			kind: "terraform-block",
		}
	}
	if block.Type != "" {
		return terraformSymbolSpec{name: block.Type, kind: "terraform-block"}
	}
	return terraformSymbolSpec{}
}

func terraformDirectStaticAttribute(attrs []treesitter.HCLAttribute, blockStart int, name string) string {
	for _, attr := range attrs {
		if attr.BlockStart == blockStart && attr.Name == name {
			return attr.StaticValue
		}
	}
	return ""
}

func terraformTopLevelBlockByStart(blocks []treesitter.HCLBlock, start int) (treesitter.HCLBlock, bool) {
	for _, block := range blocks {
		if block.TopLevel && block.Start == start {
			return block, true
		}
	}
	return treesitter.HCLBlock{}, false
}

func terraformBlockSignature(content string, block treesitter.HCLBlock) string {
	end := block.HeaderEnd
	if end <= block.Start || end > len(content) {
		end = block.End
	}
	return terraformCompactSignature(content, block.Start, end)
}

func terraformAttributeSignature(content string, attr treesitter.HCLAttribute) string {
	return terraformCompactSignature(content, attr.Start, attr.End)
}

func terraformCompactSignature(content string, start, end int) string {
	start = clampOffset(content, start)
	end = clampOffset(content, end)
	if start >= end {
		return ""
	}
	signature := strings.Join(strings.Fields(content[start:end]), " ")
	runes := []rune(signature)
	if len(runes) > 240 {
		signature = string(runes[:239]) + "…"
	}
	return signature
}

func emitTerraformRelationsTS(idx *Index, file, content string) {
	res, err := treesitter.Parse("hcl", []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	if !res.ParseOK {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "parse_error", Message: res.ParseError}})
	}

	starts := lineStarts(content)
	sourceByBlock, sourceByAttr := terraformRelationSources(idx, file, res, starts)
	targets := terraformRelationTargets(idx, file, res.HCLRefs)

	for _, ref := range res.HCLRefs {
		address := terraformReferenceAddress(ref)
		if address == "" {
			continue
		}
		targetsForAddress := targets[address]
		if len(targetsForAddress) != 1 {
			continue
		}
		from := sourceByAttr[ref.AttributeStart]
		if from == "" {
			from = sourceByBlock[ref.TopLevelBlockStart]
		}
		if from == "" {
			from = fileSymbolID(file)
		}
		raw := ""
		if ref.Start >= 0 && ref.End > ref.Start && ref.End <= len(content) {
			raw = content[ref.Start:ref.End]
		}
		line, col := offsetToLineCol(starts, ref.Start)
		idx.AddCGPEdge(
			from,
			targetsForAddress[0].ID,
			terraformDependencyEdge,
			ConfExact,
			Location{
				File: file, StartLine: line, StartColumn: col,
				EndLine: line, EndColumn: col + len(raw),
				Kind: "terraform-reference", Raw: raw,
			},
		)
	}

	emitTerraformModuleSourceEdges(idx, file, res, sourceByBlock, starts)
}

func terraformRelationSources(idx *Index, file string, res treesitter.ParseResult, starts []int) (map[int]string, map[int]string) {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	symbols := append([]CGPSymbol(nil), idx.symbolsByFile[file]...)
	idx.mu.Unlock()

	byBlock := map[int]string{}
	byAttr := map[int]string{}
	for _, block := range res.HCLBlocks {
		if !block.TopLevel || block.Type == "locals" {
			continue
		}
		spec := terraformBlockSymbolSpec(block, res.HCLAttrs)
		line, _ := offsetToLineCol(starts, block.Start)
		if sym := terraformFindSourceSymbol(symbols, spec.name, spec.kind, line); sym.ID != "" {
			byBlock[block.Start] = sym.ID
		}
	}
	for _, attr := range res.HCLAttrs {
		block, ok := terraformTopLevelBlockByStart(res.HCLBlocks, attr.TopLevelBlockStart)
		if !ok || block.Type != "locals" || attr.BlockStart != block.Start {
			continue
		}
		line, _ := offsetToLineCol(starts, attr.Start)
		if sym := terraformFindSourceSymbol(symbols, "local."+attr.Name, "terraform-local", line); sym.ID != "" {
			byAttr[attr.Start] = sym.ID
		}
	}
	return byBlock, byAttr
}

func terraformFindSourceSymbol(symbols []CGPSymbol, name, kind string, line int) CGPSymbol {
	for _, sym := range symbols {
		if sym.Name == name && sym.Kind == kind && sym.StartLine == line {
			return sym
		}
	}
	return CGPSymbol{}
}

func terraformRelationTargets(idx *Index, file string, refs []treesitter.HCLTraversal) map[string][]CGPSymbol {
	moduleDir := path.Dir(filepathToSlash(file))
	wanted := map[string][]string{}
	for _, ref := range refs {
		address := terraformReferenceAddress(ref)
		if address == "" {
			continue
		}
		if ref.AttributeName == "provider" || ref.AttributeName == "providers" {
			wanted[address] = append(wanted[address], "provider."+address)
		} else {
			wanted[address] = append(wanted[address], address)
		}
	}

	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()

	targets := map[string][]CGPSymbol{}
	for address, names := range wanted {
		seen := map[string]bool{}
		for _, name := range names {
			for _, sym := range idx.symbolsByName[name] {
				if seen[sym.ID] || sym.Language != "hcl" || !isTerraformSymbolKind(sym.Kind) || path.Dir(filepathToSlash(sym.File)) != moduleDir {
					continue
				}
				seen[sym.ID] = true
				targets[address] = append(targets[address], sym)
			}
		}
	}
	return targets
}

func terraformReferenceAddress(ref treesitter.HCLTraversal) string {
	if len(ref.Parts) == 0 {
		return ""
	}
	root := ref.Parts[0]
	if ref.AttributeName == "provider" || ref.AttributeName == "providers" {
		if len(ref.Parts) >= 2 {
			return strings.Join(ref.Parts[:2], ".")
		}
		return root
	}
	switch root {
	case "var", "local", "module":
		if len(ref.Parts) >= 2 {
			return strings.Join(ref.Parts[:2], ".")
		}
	case "data", "ephemeral", "action":
		if len(ref.Parts) >= 3 {
			return strings.Join(ref.Parts[:3], ".")
		}
	case "count", "each", "path", "terraform", "self":
		return ""
	default:
		if len(ref.Parts) >= 2 {
			return strings.Join(ref.Parts[:2], ".")
		}
	}
	return ""
}

func emitTerraformModuleSourceEdges(idx *Index, file string, res treesitter.ParseResult, sourceByBlock map[int]string, starts []int) {
	hasModule := false
	for _, block := range res.HCLBlocks {
		if block.TopLevel && block.Type == "module" {
			hasModule = true
			break
		}
	}
	if !hasModule {
		return
	}

	idx.mu.Lock()
	files := make([]string, 0, len(idx.Files))
	for candidate := range idx.Files {
		files = append(files, filepathToSlash(candidate))
	}
	idx.mu.Unlock()
	sort.Strings(files)

	for _, block := range res.HCLBlocks {
		if !block.TopLevel || block.Type != "module" {
			continue
		}
		from := sourceByBlock[block.Start]
		if from == "" {
			continue
		}
		var sourceAttr treesitter.HCLAttribute
		for _, attr := range res.HCLAttrs {
			if attr.BlockStart == block.Start && attr.Name == "source" {
				sourceAttr = attr
				break
			}
		}
		source := sourceAttr.StaticValue
		if source == "" || !strings.HasPrefix(source, "./") && !strings.HasPrefix(source, "../") {
			continue
		}
		targetDir := path.Clean(path.Join(path.Dir(filepathToSlash(file)), source))
		line, col := offsetToLineCol(starts, sourceAttr.Start)
		for _, candidate := range files {
			if path.Dir(candidate) != targetDir || !strings.HasSuffix(strings.ToLower(candidate), ".tf") {
				continue
			}
			idx.AddCGPEdge(
				from,
				fileSymbolID(candidate),
				"imports",
				ConfExact,
				Location{
					File: file, StartLine: line, StartColumn: col,
					EndLine: line, EndColumn: col + len(source),
					Kind: "terraform-module-source", Raw: source,
				},
			)
		}
	}
}
