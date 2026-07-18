package mamari

import "testing"

func TestDockerfileStagesImagesPortsCommandsAndCopies(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Dockerfile", `FROM golang:1.25 AS builder
WORKDIR /src
COPY . .
RUN go build -o /out/app ./cmd/app

FROM gcr.io/distroless/base-debian12 AS runtime
COPY --from=builder /out/app /app
EXPOSE 8080
HEALTHCHECK CMD ["/app", "health"]
ENTRYPOINT ["/app"]

FROM runtime AS smoke
COPY --from=0 /out/app /smoke-app
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Files["Dockerfile"].Language != "dockerfile" {
		t.Fatalf("Dockerfile language = %q", idx.Files["Dockerfile"].Language)
	}
	wantKinds := map[string]int{"docker-stage": 3, "container-image": 2, "container-port": 1, "container-command": 2}
	for _, sym := range idx.Symbols {
		if sym.File == "Dockerfile" {
			if _, ok := wantKinds[sym.Kind]; ok {
				wantKinds[sym.Kind]--
			}
		}
	}
	for kind, remaining := range wantKinds {
		if remaining != 0 {
			t.Fatalf("kind %s count mismatch, remaining %d", kind, remaining)
		}
	}
	for _, typ := range []string{"uses-base-image", "builds-from-stage", "copies-from-stage", "exposes-port", "runs-command"} {
		if !hasInfraEdge(idx, typ, "Dockerfile") {
			t.Fatalf("missing %s edge", typ)
		}
	}
}

func TestKubernetesMultiDocumentRelationshipsAndKustomize(t *testing.T) {
	root := t.TempDir()
	write(t, root, "base/workload.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    metadata:
      labels:
        app: api
    spec:
      serviceAccountName: api-account
      containers:
        - name: api
          image: example/api:v1
          envFrom:
            - configMapRef:
                name: api-config
            - secretRef:
                name: api-secret
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: api-data
---
apiVersion: v1
kind: Service
metadata:
  name: api
spec:
  selector:
    app: api
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: api
spec:
  rules:
    - http:
        paths:
          - backend:
              service:
                name: api
                port:
                  number: 80
`)
	write(t, root, "base/dependencies.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: api-config
---
apiVersion: v1
kind: Secret
metadata:
  name: api-secret
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: api-account
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: api-data
`)
	write(t, root, "base/autoscale.yaml", `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: api
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: api
`)
	write(t, root, "overlays/prod/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../base/workload.yaml
  - ../../base/dependencies.yaml
patches:
  - path: deployment-patch.yaml
`)
	write(t, root, "overlays/prod/deployment-patch.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resources := 0
	for _, sym := range idx.Symbols {
		if sym.Kind == "k8s-resource" {
			resources++
		}
	}
	if resources != 9 {
		t.Fatalf("k8s resources = %d, want 9 (including every YAML document)", resources)
	}
	for _, typ := range []string{
		"runs-container", "uses-image", "selects-workload", "uses-configmap", "uses-secret",
		"uses-service-account", "mounts-claim", "routes-to-service", "targets-resource",
		"includes-resource", "patches-resource",
	} {
		if !hasInfraEdge(idx, typ, "") {
			t.Fatalf("missing %s edge", typ)
		}
	}
}

func TestWatchRebakesAllInfraRelationships(t *testing.T) {
	root := t.TempDir()
	write(t, root, "service.yaml", `apiVersion: v1
kind: Service
metadata:
  name: gateway
spec:
  selector:
    app: api
`)
	write(t, root, "workloads.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    metadata:
      labels: {app: api}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
spec:
  template:
    metadata:
      labels: {app: worker}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if target := selectedWorkloadName(idx); target != "api" {
		t.Fatalf("initial selected workload = %q, want api", target)
	}
	write(t, root, "service.yaml", `apiVersion: v1
kind: Service
metadata:
  name: gateway
spec:
  selector:
    app: worker
`)
	if err := rebakeFile(idx, root, "service.yaml"); err != nil {
		t.Fatal(err)
	}
	if target := selectedWorkloadName(idx); target != "worker" {
		t.Fatalf("selected workload after rebake = %q, want worker", target)
	}
}

func selectedWorkloadName(idx *Index) string {
	for _, edge := range idx.SymbolEdges {
		if edge.Type == "selects-workload" {
			return idx.Symbols[edge.To].Name
		}
	}
	return ""
}

func hasInfraEdge(idx *Index, typ, file string) bool {
	for _, edge := range idx.SymbolEdges {
		if edge.Type == typ && (file == "" || edge.Evidence.File == file) {
			return true
		}
	}
	return false
}
