package mamari

import "testing"

func TestSearchValidationQuestionFindsValidateImplementationBeforeErrorDisplay(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/form.ts", `export class ShaclForm {
  public async validate() {
    const report = await new Validator().validate()
    return report
  }

  private createValidationErrorDisplay(result: unknown) {
    return String(result)
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(idx, "How does SHACL form validation work?", SearchCodeOptions{Limit: 5, BudgetTokens: 800})
	if resp.Status != "ok" || len(resp.Hits) == 0 {
		t.Fatalf("search failed: %#v", resp)
	}
	foundValidate := false
	for _, symbol := range resp.Hits[0].Symbols {
		if symbol.Name == "validate" {
			foundValidate = true
		}
	}
	if !foundValidate {
		t.Fatalf("top hit should be owned by validate, got %#v", resp.Hits[0])
	}
}
