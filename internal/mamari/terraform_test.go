package mamari

import (
	"strings"
	"testing"
)

func TestTerraformNativeSyntaxIndexesAddressesAndDependencies(t *testing.T) {
	root := t.TempDir()
	write(t, root, "variables.tf", `variable "region" {
  description = "AWS region for the deployment"
  type        = string
}
`)
	write(t, root, "locals.tf", `locals {
  prefix = "${var.region}-app"
}
`)
	write(t, root, "providers.tf", `provider "aws" {
  region = var.region
}

provider "aws" {
  alias  = "west"
  region = "us-west-2"
}
`)
	write(t, root, "data.tf", `data "aws_ami" "ubuntu" {
  owners = [var.region]
}
`)
	write(t, root, "main.tf", `resource "aws_security_group" "web" {}

ephemeral "random_password" "db" {
  length = 32
}

action "aws_lambda_invoke" "restart" {
  config {
    payload = local.prefix
  }
}

resource "aws_instance" "web" {
  ami        = data.aws_ami.ubuntu.id
  subnet_id  = module.network.subnet_id
  name       = local.prefix
  password   = ephemeral.random_password.db.result
  provider   = aws.west
  depends_on = [aws_security_group.web]

  lifecycle {
    action_trigger {
      actions = [action.aws_lambda_invoke.restart]
    }
  }
}
`)
	write(t, root, "modules/network/main.tf", `resource "aws_subnet" "main" {
  cidr_block = "10.0.0.0/24"
}

output "subnet_id" {
  value = aws_subnet.main.id
}
`)
	write(t, root, "modules/network/duplicate.tf", `resource "aws_instance" "web" {}

output "web_id" {
  value = aws_instance.web.id
}
`)
	write(t, root, "module.tf", `module "network" {
  source    = "./modules/network"
  region    = var.region
  providers = { aws = aws.west }
}
`)
	write(t, root, "outputs.tf", `output "web_id" {
  description = "Created instance ID"
  value       = aws_instance.web.id
}
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	if file := idx.Files["main.tf"]; file.Language != "hcl" || file.Parser != "tree-sitter-hcl" || file.ParseStatus != ParseStatusOK {
		t.Fatalf("unexpected Terraform file metadata: %+v", file)
	}

	wantSymbols := []struct {
		name, kind, file string
	}{
		{"var.region", "terraform-variable", "variables.tf"},
		{"local.prefix", "terraform-local", "locals.tf"},
		{"provider.aws", "terraform-provider", "providers.tf"},
		{"provider.aws.west", "terraform-provider", "providers.tf"},
		{"data.aws_ami.ubuntu", "terraform-data", "data.tf"},
		{"aws_security_group.web", "terraform-resource", "main.tf"},
		{"ephemeral.random_password.db", "terraform-ephemeral", "main.tf"},
		{"action.aws_lambda_invoke.restart", "terraform-action", "main.tf"},
		{"aws_instance.web", "terraform-resource", "main.tf"},
		{"module.network", "terraform-module", "module.tf"},
		{"output.web_id", "terraform-output", "outputs.tf"},
	}
	found := map[string]CGPSymbol{}
	for _, want := range wantSymbols {
		sym := requireTerraformSymbol(t, idx, want.name, want.kind, want.file)
		found[want.file+"\x00"+want.name] = sym
		if want.kind == "terraform-resource" && !strings.HasPrefix(sym.Signature, "resource ") {
			t.Fatalf("expected useful Terraform signature for %s, got %q", want.name, sym.Signature)
		}
	}
	if got := found["variables.tf\x00var.region"].Docstring; got != "AWS region for the deployment" {
		t.Fatalf("expected variable description as docstring, got %q", got)
	}
	if got := found["outputs.tf\x00output.web_id"].Docstring; got != "Created instance ID" {
		t.Fatalf("expected output description as docstring, got %q", got)
	}

	web := found["main.tf\x00aws_instance.web"]
	requireTerraformEdge(t, idx, web.ID, found["data.tf\x00data.aws_ami.ubuntu"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, web.ID, found["locals.tf\x00local.prefix"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, web.ID, found["main.tf\x00ephemeral.random_password.db"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, web.ID, found["providers.tf\x00provider.aws.west"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, web.ID, found["main.tf\x00aws_security_group.web"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, web.ID, found["main.tf\x00action.aws_lambda_invoke.restart"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, web.ID, found["module.tf\x00module.network"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, found["locals.tf\x00local.prefix"].ID, found["variables.tf\x00var.region"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, found["data.tf\x00data.aws_ami.ubuntu"].ID, found["variables.tf\x00var.region"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, found["module.tf\x00module.network"].ID, found["variables.tf\x00var.region"].ID, terraformDependencyEdge)
	requireTerraformEdge(t, idx, found["module.tf\x00module.network"].ID, found["providers.tf\x00provider.aws.west"].ID, terraformDependencyEdge)
	if hasTerraformEdge(idx, found["module.tf\x00module.network"].ID, found["providers.tf\x00provider.aws"].ID, terraformDependencyEdge) {
		t.Fatal("providers map key was misread as a reference to the default provider")
	}
	requireTerraformEdge(t, idx, found["outputs.tf\x00output.web_id"].ID, web.ID, terraformDependencyEdge)

	childWeb := requireTerraformSymbol(t, idx, "aws_instance.web", "terraform-resource", "modules/network/duplicate.tf")
	childOutput := requireTerraformSymbol(t, idx, "output.web_id", "terraform-output", "modules/network/duplicate.tf")
	requireTerraformEdge(t, idx, childOutput.ID, childWeb.ID, terraformDependencyEdge)
	if hasTerraformEdge(idx, childOutput.ID, web.ID, terraformDependencyEdge) {
		t.Fatal("child-module reference incorrectly resolved to the root module's same-address resource")
	}

	networkModule := found["module.tf\x00module.network"]
	requireTerraformEdge(t, idx, networkModule.ID, fileSymbolID("modules/network/main.tf"), "imports")
	requireTerraformEdge(t, idx, networkModule.ID, fileSymbolID("modules/network/duplicate.tf"), "imports")

	trace := TraceSymbol(idx, "var.region")
	if trace.Status != "found" || !summaryContainsTerraformSymbol(trace.Callers, "local.prefix") || !summaryContainsTerraformSymbol(trace.Callers, "module.network") {
		t.Fatalf("expected Terraform dependency callers in trace, got %+v", trace)
	}
	impact := Impact(idx, "var.region", 1)
	if impact.Status != "found" || len(impact.Layers) != 1 || !impactLayerContainsTerraformSymbol(impact.Layers[0], "local.prefix") {
		t.Fatalf("expected Terraform dependency blast radius, got %+v", impact)
	}
}

func TestGenericHCLKeepsGenericBlockBehavior(t *testing.T) {
	root := t.TempDir()
	write(t, root, "service.hcl", `service "api" {
  port = 8080
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range idx.Symbols {
		if sym.File == "service.hcl" && sym.Name == "api" && sym.Kind == "class" {
			return
		}
	}
	t.Fatalf("expected non-Terraform .hcl to retain generic HCL symbols, got %+v", idx.Symbols)
}

func TestTerraformWatchRebakeKeepsDependenciesAndModuleImports(t *testing.T) {
	root := t.TempDir()
	write(t, root, "variables.tf", `variable "region" { type = string }
`)
	write(t, root, "locals.tf", `locals {
  prefix = var.region
}
`)
	write(t, root, "main.tf", `resource "aws_instance" "web" {
  name = local.prefix
}
`)
	write(t, root, "module.tf", `module "child" {
  source = "./child"
}
`)
	write(t, root, "child/main.tf", `resource "aws_subnet" "main" {}
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	assertGraph := func() {
		variable := requireTerraformSymbol(t, idx, "var.region", "terraform-variable", "variables.tf")
		local := requireTerraformSymbol(t, idx, "local.prefix", "terraform-local", "locals.tf")
		resource := requireTerraformSymbol(t, idx, "aws_instance.web", "terraform-resource", "main.tf")
		module := requireTerraformSymbol(t, idx, "module.child", "terraform-module", "module.tf")
		requireTerraformEdge(t, idx, local.ID, variable.ID, terraformDependencyEdge)
		requireTerraformEdge(t, idx, resource.ID, local.ID, terraformDependencyEdge)
		requireTerraformEdge(t, idx, module.ID, fileSymbolID("child/main.tf"), "imports")
	}
	assertGraph()

	write(t, root, "variables.tf", `variable "region" {
  description = "updated"
  type        = string
}
`)
	if err := rebakeFile(idx, root, "variables.tf"); err != nil {
		t.Fatal(err)
	}
	assertGraph()

	write(t, root, "child/main.tf", `resource "aws_subnet" "main" {
  cidr_block = "10.0.0.0/24"
}
`)
	if err := rebakeFile(idx, root, "child/main.tf"); err != nil {
		t.Fatal(err)
	}
	assertGraph()
}

func requireTerraformSymbol(t *testing.T, idx *Index, name, kind, file string) CGPSymbol {
	t.Helper()
	for _, sym := range idx.Symbols {
		if sym.Name == name && sym.Kind == kind && sym.File == file {
			return sym
		}
	}
	t.Fatalf("missing Terraform symbol %s (%s) in %s", name, kind, file)
	return CGPSymbol{}
}

func requireTerraformEdge(t *testing.T, idx *Index, from, to, edgeType string) {
	t.Helper()
	if !hasTerraformEdge(idx, from, to, edgeType) {
		t.Fatalf("missing Terraform edge %s -[%s]-> %s; edges=%+v", from, edgeType, to, idx.SymbolEdges)
	}
}

func hasTerraformEdge(idx *Index, from, to, edgeType string) bool {
	for _, edge := range idx.SymbolEdges {
		if edge.From == from && edge.To == to && edge.Type == edgeType && edge.Confidence == ConfExact {
			return true
		}
	}
	return false
}

func summaryContainsTerraformSymbol(symbols []CGPSymbolSummary, name string) bool {
	for _, sym := range symbols {
		if sym.Name == name {
			return true
		}
	}
	return false
}

func impactLayerContainsTerraformSymbol(layer ImpactLayer, name string) bool {
	for _, sym := range layer.Symbols {
		if sym.Name == name {
			return true
		}
	}
	return false
}
