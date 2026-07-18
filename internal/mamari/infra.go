package mamari

import (
	"bufio"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type infraRef struct {
	kind string
	name string
	line int
}

type infraResource struct {
	id        string
	file      string
	kind      string
	name      string
	namespace string
	line      int
	labels    map[string]string
	selector  map[string]string
	refs      []infraRef
}

type infraPathRef struct {
	path string
	line int
}

type infraKustomization struct {
	id         string
	file       string
	resources  []infraPathRef
	patches    []infraPathRef
	components []infraPathRef
}

type dockerCopy struct {
	stage string
	line  int
}

type infraDockerfile struct {
	file        string
	stageByName map[string]string
	copies      []dockerCopy
}

func emitYAMLInfraSymbols(idx *Index, file, content, parentID string) {
	decoder := yaml.NewDecoder(strings.NewReader(content))
	docNumber := 0
	for {
		var doc yaml.Node
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		if len(doc.Content) == 0 {
			continue
		}
		docNumber++
		root := doc.Content[0]
		if isKustomizationFile(file) || yamlScalar(root, "kind") == "Kustomization" {
			emitKustomizationSymbol(idx, file, root, parentID)
			continue
		}
		emitK8sResourceNode(idx, file, root, parentID, docNumber)
	}
}

func emitK8sResourceNode(idx *Index, file string, node *yaml.Node, parentID string, ordinal int) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	kind := yamlScalar(node, "kind")
	if kind == "List" {
		if items := yamlMapValue(node, "items"); items != nil && items.Kind == yaml.SequenceNode {
			for i, item := range items.Content {
				emitK8sResourceNode(idx, file, item, parentID, ordinal*1000+i+1)
			}
		}
		return
	}
	if yamlScalar(node, "apiVersion") == "" || kind == "" {
		return
	}
	metadata := yamlMapValue(node, "metadata")
	name := yamlScalar(metadata, "name")
	if name == "" {
		return
	}
	namespace := yamlScalar(metadata, "namespace")
	if namespace == "" {
		namespace = "default"
	}
	qualified := fmt.Sprintf("%s/%s#%d", kind, name, ordinal)
	sym := idx.AddCGPSymbol(CGPSymbol{
		ID: stableSymbolID("infra", "k8s-resource", file, qualified, idx), Name: name,
		Kind: "k8s-resource", Language: "yaml", File: file,
		StartLine: node.Line, StartColumn: maxInt(node.Column, 1), EndLine: yamlNodeEndLine(node), EndColumn: 1,
		Signature: kind + " " + namespace + "/" + name, ParentID: parentID, Confidence: ConfExact,
	})
	res := infraResource{
		id: sym.ID, file: file, kind: kind, name: name, namespace: namespace, line: node.Line,
		labels: yamlStringMap(yamlMapValue(metadata, "labels")), refs: collectK8sRefs(node),
	}
	spec := yamlMapValue(node, "spec")
	if kind == "Service" {
		res.selector = yamlStringMap(yamlMapValue(spec, "selector"))
	}
	template := yamlMapValue(spec, "template")
	if kind == "CronJob" {
		template = yamlMapValue(yamlMapValue(yamlMapValue(spec, "jobTemplate"), "spec"), "template")
	}
	if template != nil {
		res.labels = yamlStringMap(yamlMapValue(yamlMapValue(template, "metadata"), "labels"))
		emitK8sContainers(idx, file, yamlMapValue(template, "spec"), sym)
	} else if kind == "Pod" {
		emitK8sContainers(idx, file, spec, sym)
	}
	idx.mu.Lock()
	if idx.infraResources == nil {
		idx.infraResources = map[string]infraResource{}
	}
	idx.infraResources[sym.ID] = res
	idx.mu.Unlock()
}

func emitK8sContainers(idx *Index, file string, podSpec *yaml.Node, resource CGPSymbol) {
	for _, field := range []string{"initContainers", "containers"} {
		seq := yamlMapValue(podSpec, field)
		if seq == nil || seq.Kind != yaml.SequenceNode {
			continue
		}
		for i, container := range seq.Content {
			name := yamlScalar(container, "name")
			if name == "" {
				name = fmt.Sprintf("%s-%d", strings.TrimSuffix(field, "s"), i+1)
			}
			child := idx.AddCGPSymbol(CGPSymbol{
				ID: stableSymbolID("infra", "container", file, resource.ID+"/"+name, idx), Name: name,
				Kind: "container", Language: "yaml", File: file, StartLine: container.Line,
				StartColumn: maxInt(container.Column, 1), EndLine: yamlNodeEndLine(container), EndColumn: 1,
				Signature: strings.TrimSpace(name + " " + yamlScalar(container, "image")), ParentID: resource.ID, Confidence: ConfExact,
			})
			idx.AddCGPEdge(resource.ID, child.ID, "runs-container", ConfExact, infraLocation(file, container.Line, name))
			if image := yamlScalar(container, "image"); image != "" {
				imageID := addInfraExternalSymbol(idx, file, "container-image", image, resource.ID, container.Line)
				idx.AddCGPEdge(child.ID, imageID, "uses-image", ConfExact, infraLocation(file, container.Line, image))
			}
		}
	}
}

func emitKustomizationSymbol(idx *Index, file string, root *yaml.Node, parentID string) {
	name := path.Base(path.Dir(file))
	if name == "." || name == "/" || name == "" {
		name = path.Base(file)
	}
	sym := idx.AddCGPSymbol(CGPSymbol{
		ID: stableSymbolID("infra", "kustomization", file, name, idx), Name: name,
		Kind: "kustomization", Language: "yaml", File: file, StartLine: maxInt(root.Line, 1), StartColumn: 1,
		EndLine: yamlNodeEndLine(root), EndColumn: 1, Signature: "Kustomization " + name, ParentID: parentID, Confidence: ConfExact,
	})
	k := infraKustomization{id: sym.ID, file: file}
	k.resources = append(k.resources, yamlPathRefs(root, "resources")...)
	k.resources = append(k.resources, yamlPathRefs(root, "bases")...)
	k.components = yamlPathRefs(root, "components")
	k.patches = append(k.patches, yamlPathRefs(root, "patchesStrategicMerge")...)
	k.patches = append(k.patches, yamlPathRefs(root, "patchesJson6902")...)
	k.patches = append(k.patches, yamlPathRefs(root, "patches")...)
	idx.mu.Lock()
	if idx.infraKustomizations == nil {
		idx.infraKustomizations = map[string]infraKustomization{}
	}
	idx.infraKustomizations[file] = k
	idx.mu.Unlock()
}

func emitYAMLInfraRelations(idx *Index, file string) {
	idx.mu.Lock()
	resources := make([]infraResource, 0, len(idx.infraResources))
	for _, resource := range idx.infraResources {
		resources = append(resources, resource)
	}
	kustom, hasKustom := idx.infraKustomizations[file]
	idx.mu.Unlock()
	sort.Slice(resources, func(i, j int) bool { return resources[i].id < resources[j].id })

	for _, source := range resources {
		if source.file != file {
			continue
		}
		if source.kind == "Service" && len(source.selector) > 0 {
			for _, target := range resources {
				if source.namespace == target.namespace && isK8sWorkload(target.kind) && labelsContain(target.labels, source.selector) {
					idx.AddCGPEdge(source.id, target.id, "selects-workload", ConfExact, infraLocation(file, source.line, source.name))
				}
			}
		}
		for _, ref := range source.refs {
			for _, target := range resources {
				if target.namespace != source.namespace || target.kind != ref.kind || target.name != ref.name {
					continue
				}
				edgeType := k8sRefEdgeType(ref.kind)
				idx.AddCGPEdge(source.id, target.id, edgeType, ConfExact, infraLocation(file, ref.line, ref.name))
			}
		}
	}
	if hasKustom {
		for _, group := range []struct {
			refs []infraPathRef
			typ  string
		}{{kustom.resources, "includes-resource"}, {kustom.components, "includes-component"}, {kustom.patches, "patches-resource"}} {
			for _, ref := range group.refs {
				if target := resolveInfraPath(idx, file, ref.path); target != "" {
					idx.AddCGPEdge(kustom.id, fileSymbolID(target), group.typ, ConfExact, infraLocation(file, ref.line, ref.path))
				}
			}
		}
	}
}

func collectK8sRefs(root *yaml.Node) []infraRef {
	var out []infraRef
	var walk func(*yaml.Node)
	walk = func(node *yaml.Node) {
		if node == nil {
			return
		}
		if node.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(node.Content); i += 2 {
				key, value := node.Content[i].Value, node.Content[i+1]
				kind, name := "", ""
				switch key {
				case "configMapRef", "configMapKeyRef", "configMap":
					kind, name = "ConfigMap", yamlScalar(value, "name")
				case "secretRef", "secretKeyRef":
					kind, name = "Secret", yamlScalar(value, "name")
				case "secret":
					kind, name = "Secret", yamlScalar(value, "secretName")
				case "persistentVolumeClaim":
					kind, name = "PersistentVolumeClaim", yamlScalar(value, "claimName")
				case "scaleTargetRef":
					kind, name = yamlScalar(value, "kind"), yamlScalar(value, "name")
				case "serviceAccountName":
					kind, name = "ServiceAccount", value.Value
				}
				if kind != "" && name != "" {
					out = append(out, infraRef{kind: kind, name: name, line: value.Line})
				}
				if key == "backend" {
					if service := yamlMapValue(value, "service"); service != nil {
						if serviceName := yamlScalar(service, "name"); serviceName != "" {
							out = append(out, infraRef{kind: "Service", name: serviceName, line: service.Line})
						}
					}
				}
				walk(value)
			}
			return
		}
		for _, child := range node.Content {
			walk(child)
		}
	}
	walk(root)
	return out
}

func k8sRefEdgeType(kind string) string {
	switch kind {
	case "ConfigMap":
		return "uses-configmap"
	case "Secret":
		return "uses-secret"
	case "PersistentVolumeClaim":
		return "mounts-claim"
	case "ServiceAccount":
		return "uses-service-account"
	case "Service":
		return "routes-to-service"
	default:
		return "targets-resource"
	}
}

func isK8sWorkload(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob", "Pod":
		return true
	default:
		return false
	}
}

func labelsContain(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func emitDockerfileSymbols(idx *Index, file, content, parentID string) {
	logical := dockerLogicalLines(content)
	state := infraDockerfile{file: file, stageByName: map[string]string{}}
	var currentStage string
	stageNumber := 0
	for _, line := range logical {
		fields := strings.Fields(line.text)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			var args []string
			for _, field := range fields[1:] {
				if !strings.HasPrefix(field, "--") {
					args = append(args, field)
				}
			}
			if len(args) == 0 {
				continue
			}
			image, name := args[0], args[0]
			baseStage := state.stageByName[image]
			if len(args) >= 3 && strings.EqualFold(args[len(args)-2], "AS") {
				name = args[len(args)-1]
			}
			stage := idx.AddCGPSymbol(CGPSymbol{
				ID: stableSymbolID("infra", "docker-stage", file, name, idx), Name: name, Kind: "docker-stage",
				Language: "dockerfile", File: file, StartLine: line.line, StartColumn: 1, EndLine: line.line,
				EndColumn: len(line.text) + 1, Signature: line.text, ParentID: parentID, Confidence: ConfExact,
			})
			currentStage = stage.ID
			state.stageByName[name] = stage.ID
			state.stageByName[fmt.Sprint(stageNumber)] = stage.ID
			stageNumber++
			if baseStage != "" && baseStage != stage.ID {
				idx.AddCGPEdge(stage.ID, baseStage, "builds-from-stage", ConfExact, infraLocation(file, line.line, image))
			} else {
				imageID := addInfraExternalSymbol(idx, file, "container-image", image, stage.ID, line.line)
				idx.AddCGPEdge(stage.ID, imageID, "uses-base-image", ConfExact, infraLocation(file, line.line, image))
			}
		case "COPY":
			for _, field := range fields[1:] {
				if strings.HasPrefix(strings.ToLower(field), "--from=") {
					state.copies = append(state.copies, dockerCopy{stage: field[strings.IndexByte(field, '=')+1:], line: line.line})
				}
			}
		case "EXPOSE":
			for _, port := range fields[1:] {
				id := addInfraExternalSymbol(idx, file, "container-port", port, currentStage, line.line)
				if currentStage != "" {
					idx.AddCGPEdge(currentStage, id, "exposes-port", ConfExact, infraLocation(file, line.line, port))
				}
			}
		case "CMD", "ENTRYPOINT", "HEALTHCHECK":
			name := strings.ToLower(fields[0])
			command := idx.AddCGPSymbol(CGPSymbol{
				ID:   stableSymbolID("infra", "container-command", file, fmt.Sprintf("%s#%d", name, line.line), idx),
				Name: name, Kind: "container-command", Language: "dockerfile", File: file,
				StartLine: line.line, StartColumn: 1, EndLine: line.line, EndColumn: len(line.text) + 1,
				Signature: line.text, ParentID: currentStage, Confidence: ConfExact,
			})
			if currentStage != "" {
				idx.AddCGPEdge(currentStage, command.ID, "runs-command", ConfExact, infraLocation(file, line.line, line.text))
			}
		}
	}
	idx.mu.Lock()
	if idx.infraDockerfiles == nil {
		idx.infraDockerfiles = map[string]infraDockerfile{}
	}
	idx.infraDockerfiles[file] = state
	idx.mu.Unlock()
}

func emitDockerfileRelations(idx *Index, file string) {
	idx.mu.Lock()
	state, ok := idx.infraDockerfiles[file]
	idx.mu.Unlock()
	if !ok {
		return
	}
	for _, copy := range state.copies {
		to := state.stageByName[copy.stage]
		if to == "" {
			continue
		}
		from := dockerStageAtLine(idx, file, copy.line)
		if from != "" && from != to {
			idx.AddCGPEdge(from, to, "copies-from-stage", ConfExact, infraLocation(file, copy.line, copy.stage))
		}
	}
}

func dockerStageAtLine(idx *Index, file string, line int) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	best, bestLine := "", -1
	for _, sym := range idx.Symbols {
		if sym.File == file && sym.Kind == "docker-stage" && sym.StartLine <= line && sym.StartLine > bestLine {
			best, bestLine = sym.ID, sym.StartLine
		}
	}
	return best
}

type dockerLine struct {
	line int
	text string
}

func dockerLogicalLines(content string) []dockerLine {
	scanner := bufio.NewScanner(strings.NewReader(content))
	var out []dockerLine
	lineNo, start := 0, 0
	var current strings.Builder
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if current.Len() == 0 {
			start = lineNo
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		continued := strings.HasSuffix(line, "\\")
		line = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		current.WriteString(line)
		if continued {
			continue
		}
		out = append(out, dockerLine{line: start, text: current.String()})
		current.Reset()
	}
	if current.Len() > 0 {
		out = append(out, dockerLine{line: start, text: current.String()})
	}
	return out
}

func addInfraExternalSymbol(idx *Index, file, kind, name, parentID string, line int) string {
	sym := idx.AddCGPSymbol(CGPSymbol{
		ID: stableSymbolID("infra", kind, file, name, idx), Name: name, Kind: kind,
		Language: "infra", File: file, StartLine: line, StartColumn: 1, EndLine: line, EndColumn: 1,
		Signature: name, ParentID: parentID, Confidence: ConfExact,
	})
	return sym.ID
}

func resolveInfraPath(idx *Index, fromFile, ref string) string {
	if ref == "" || strings.Contains(ref, "://") {
		return ""
	}
	base := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(fromFile), ref)))
	candidates := []string{base}
	if filepath.Ext(base) == "" {
		candidates = append(candidates, path.Join(base, "kustomization.yaml"), path.Join(base, "kustomization.yml"))
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, candidate := range candidates {
		if _, ok := idx.Files[candidate]; ok {
			return candidate
		}
	}
	return ""
}

func yamlPathRefs(root *yaml.Node, key string) []infraPathRef {
	node := yamlMapValue(root, key)
	if node == nil {
		return nil
	}
	var out []infraPathRef
	if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			if item.Kind == yaml.ScalarNode {
				out = append(out, infraPathRef{path: item.Value, line: item.Line})
			} else if p := yamlScalar(item, "path"); p != "" {
				out = append(out, infraPathRef{path: p, line: item.Line})
			}
		}
	}
	return out
}

func yamlMapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func yamlScalar(node *yaml.Node, key string) string {
	value := yamlMapValue(node, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return ""
	}
	return value.Value
}

func yamlStringMap(node *yaml.Node) map[string]string {
	out := map[string]string{}
	if node == nil || node.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i+1].Kind == yaml.ScalarNode {
			out[node.Content[i].Value] = node.Content[i+1].Value
		}
	}
	return out
}

func yamlNodeEndLine(node *yaml.Node) int {
	if node == nil {
		return 1
	}
	end := maxInt(node.Line, 1)
	for _, child := range node.Content {
		if childEnd := yamlNodeEndLine(child); childEnd > end {
			end = childEnd
		}
	}
	return end
}

func isKustomizationFile(file string) bool {
	base := strings.ToLower(filepath.Base(file))
	return base == "kustomization.yaml" || base == "kustomization.yml"
}

func infraLocation(file string, line int, raw string) Location {
	return Location{File: file, StartLine: maxInt(line, 1), StartColumn: 1, EndLine: maxInt(line, 1), EndColumn: len(raw) + 1, Kind: "infra", Raw: raw}
}
