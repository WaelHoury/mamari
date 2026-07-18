package mamari

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	scip "github.com/sourcegraph/scip/bindings/go/scip"
)

type pendingSCIPEdge struct {
	fromSymbol string
	toSCIP     string
	edgeType   string
	loc        Location
}

// IngestSCIP adds compiler-backed SCIP symbols and reference/import edges to
// an existing Mamari index. SCIP references are intentionally not translated
// into "calls": an occurrence may be an import, type annotation, variable
// read, property reference, or other non-call usage.
func IngestSCIP(idx *Index, r io.Reader) error {
	// Treat the complete import as one graph transaction. Existing readers
	// retain the last complete generation until every SCIP symbol/edge has
	// been applied and the replacement is published below.
	idx.beginSymbolGraphMutation(true)
	symbolBySCIP := existingSCIPSymbolMap(idx)
	var pending []pendingSCIPEdge
	visitor := &scip.IndexVisitor{
		VisitDocument: func(ctx context.Context, doc *scip.Document) error {
			rel := filepath.ToSlash(filepath.Clean(doc.GetRelativePath()))
			if rel == "." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
				return nil
			}
			defRanges := map[string]scipRange{}
			for _, occ := range doc.GetOccurrences() {
				if occ == nil || occ.GetSymbol() == "" {
					continue
				}
				if scipRoleMatches(occ, scip.SymbolRole_Definition) || scipRoleMatches(occ, scip.SymbolRole_ForwardDefinition) {
					defRanges[occ.GetSymbol()] = parseSCIPRange(occ.GetRange())
				}
			}
			for _, info := range doc.GetSymbols() {
				if info == nil || info.GetSymbol() == "" {
					continue
				}
				sym := scipSymbolFromInfo(idx, rel, info, defRanges[info.GetSymbol()])
				if sym.ID == "" {
					continue
				}
				added := idx.AddCGPSymbol(sym)
				symbolBySCIP[info.GetSymbol()] = added.ID
				for _, reln := range info.GetRelationships() {
					if reln == nil || reln.GetSymbol() == "" {
						continue
					}
					if reln.GetIsReference() || reln.GetIsDefinition() || reln.GetIsImplementation() || reln.GetIsTypeDefinition() {
						pending = append(pending, pendingSCIPEdge{
							fromSymbol: added.ID,
							toSCIP:     reln.GetSymbol(),
							edgeType:   "references-symbol",
							loc:        Location{File: rel, StartLine: sym.StartLine, StartColumn: sym.StartColumn, EndLine: sym.StartLine, EndColumn: sym.StartColumn, Kind: "scip-relationship", Raw: reln.GetSymbol()},
						})
					}
				}
			}
			idx.invalidateFileSymbolIndex()
			idx.ensureFileSymbolIndex()
			for _, occ := range doc.GetOccurrences() {
				if occ == nil || occ.GetSymbol() == "" {
					continue
				}
				if scipRoleMatches(occ, scip.SymbolRole_Definition) || scipRoleMatches(occ, scip.SymbolRole_ForwardDefinition) {
					if _, ok := symbolBySCIP[occ.GetSymbol()]; !ok {
						rng := parseSCIPRange(occ.GetRange())
						sym := scipSymbolFromOccurrence(idx, rel, occ.GetSymbol(), rng)
						added := idx.AddCGPSymbol(sym)
						if added.ID != "" {
							symbolBySCIP[occ.GetSymbol()] = added.ID
						}
					}
					continue
				}
				rng := parseSCIPRange(occ.GetRange())
				line := rng.startLine
				if line <= 0 {
					line = 1
				}
				from := idx.containingSymbolFast(rel, line)
				if from.ID == "" {
					from.ID = fileSymbolID(rel)
				}
				edgeType := "references-symbol"
				kind := "scip-reference"
				if scipRoleMatches(occ, scip.SymbolRole_Import) {
					edgeType = "scip-imports"
					kind = "scip-import"
				}
				pending = append(pending, pendingSCIPEdge{
					fromSymbol: from.ID,
					toSCIP:     occ.GetSymbol(),
					edgeType:   edgeType,
					loc: Location{
						File: rel, StartLine: line, StartColumn: rng.startColumn,
						EndLine: rng.endLine, EndColumn: rng.endColumn,
						Kind: kind, Raw: scipDisplayName(occ.GetSymbol()),
					},
				})
			}
			return nil
		},
		VisitExternalSymbol: func(ctx context.Context, info *scip.SymbolInformation) error {
			if info == nil || info.GetSymbol() == "" {
				return nil
			}
			sym := scipSymbolFromInfo(idx, "", info, scipRange{startLine: 1, startColumn: 1, endLine: 1, endColumn: 1})
			added := idx.AddCGPSymbol(sym)
			if added.ID != "" {
				symbolBySCIP[info.GetSymbol()] = added.ID
			}
			return nil
		},
	}
	if err := visitor.ParseStreaming(context.Background(), r); err != nil {
		return err
	}
	for _, edge := range pending {
		target := symbolBySCIP[edge.toSCIP]
		if target == "" || edge.fromSymbol == "" || target == edge.fromSymbol {
			continue
		}
		idx.AddCGPEdge(edge.fromSymbol, target, edge.edgeType, ConfExact, edge.loc)
	}
	idx.invalidateFileSymbolIndex()
	idx.invalidateCodeSearchIndex()
	sortCGP(idx)
	idx.publishSymbolGraph()
	return nil
}

type scipRange struct {
	startLine   int
	startColumn int
	endLine     int
	endColumn   int
}

func parseSCIPRange(raw []int32) scipRange {
	switch len(raw) {
	case 3:
		line := int(raw[0]) + 1
		return scipRange{startLine: line, startColumn: int(raw[1]) + 1, endLine: line, endColumn: int(raw[2]) + 1}
	case 4:
		return scipRange{startLine: int(raw[0]) + 1, startColumn: int(raw[1]) + 1, endLine: int(raw[2]) + 1, endColumn: int(raw[3]) + 1}
	default:
		return scipRange{startLine: 1, startColumn: 1, endLine: 1, endColumn: 1}
	}
}

func scipRoleMatches(occ *scip.Occurrence, role scip.SymbolRole) bool {
	return occ != nil && occ.GetSymbolRoles()&int32(role) != 0
}

func scipSymbolFromInfo(idx *Index, file string, info *scip.SymbolInformation, rng scipRange) CGPSymbol {
	name := strings.TrimSpace(info.GetDisplayName())
	if name == "" {
		name = scipDisplayName(info.GetSymbol())
	}
	kind := scipKind(info.GetKind())
	if file == "" {
		file = "external/scip"
	}
	if rng.startLine <= 0 {
		rng = scipRange{startLine: 1, startColumn: 1, endLine: 1, endColumn: 1}
	}
	return CGPSymbol{
		ID:          scipSymbolID(file, info.GetSymbol()),
		Name:        name,
		Kind:        kind,
		Language:    languageFor(file),
		File:        file,
		StartLine:   rng.startLine,
		StartColumn: rng.startColumn,
		EndLine:     rng.endLine,
		EndColumn:   rng.endColumn,
		Signature:   scipSignature(info),
		Confidence:  ConfExact,
		SCIPSymbol:  info.GetSymbol(),
	}
}

func scipSymbolFromOccurrence(idx *Index, file, symbol string, rng scipRange) CGPSymbol {
	return CGPSymbol{
		ID:          scipSymbolID(file, symbol),
		Name:        scipDisplayName(symbol),
		Kind:        "symbol",
		Language:    languageFor(file),
		File:        file,
		StartLine:   rng.startLine,
		StartColumn: rng.startColumn,
		EndLine:     rng.endLine,
		EndColumn:   rng.endColumn,
		Confidence:  ConfExact,
		SCIPSymbol:  symbol,
	}
}

func existingSCIPSymbolMap(idx *Index) map[string]string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	out := map[string]string{}
	for id, sym := range idx.Symbols {
		if sym.SCIPSymbol != "" {
			out[sym.SCIPSymbol] = id
		}
	}
	return out
}

func scipSymbolID(file, symbol string) string {
	return fmt.Sprintf("symbol:scip:%s:%s", filepath.ToSlash(file), stableHash(symbol)[:16])
}

func stableHash(s string) string {
	return hash([]byte(s))
}

func scipDisplayName(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "scip-symbol"
	}
	parts := strings.Fields(symbol)
	if len(parts) > 0 {
		symbol = parts[len(parts)-1]
	}
	symbol = strings.TrimRight(symbol, "/.#:!")
	if slash := strings.LastIndex(symbol, "/"); slash >= 0 {
		symbol = symbol[slash+1:]
	}
	for _, sep := range []string{"#", ".", ":", "!"} {
		if idx := strings.LastIndex(symbol, sep); idx >= 0 && idx+1 < len(symbol) {
			symbol = symbol[idx+1:]
		}
	}
	if symbol == "" {
		return "scip-symbol"
	}
	return symbol
}

func scipKind(kind scip.SymbolInformation_Kind) string {
	value := strings.TrimPrefix(kind.String(), "SymbolInformation_")
	value = strings.ToLower(value)
	switch value {
	case "function":
		return "function"
	case "method", "staticmethod", "traitmethod", "protocolmethod":
		return "method"
	case "class":
		return "class"
	case "interface":
		return "interface"
	case "type", "typealias":
		return "type"
	case "enum":
		return "enum"
	case "constant":
		return "constant"
	case "module", "namespace", "package":
		return "module"
	case "file":
		return "file"
	default:
		if value == "" || value == "unspecifiedkind" {
			return "symbol"
		}
		return strings.ReplaceAll(value, "_", "-")
	}
}

func scipSignature(info *scip.SymbolInformation) string {
	if info == nil {
		return ""
	}
	if sig := info.GetSignatureDocumentation(); sig != nil && sig.GetText() != "" {
		return firstLine(sig.GetText())
	}
	docs := append([]string(nil), info.GetDocumentation()...)
	sort.Strings(docs)
	if len(docs) == 0 {
		return ""
	}
	return firstLine(docs[0])
}
