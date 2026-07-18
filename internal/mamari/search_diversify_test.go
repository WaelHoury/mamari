package mamari

import "testing"

// A large file with many strong (name-matching) hits should not monopolize the
// lean search top-N when DiversifyFiles is set: the per-file cap frees slots for
// smaller but relevant files, without under-filling the limit relative to the
// un-diversified control. With the option off, behavior is unchanged (the big
// file may take every slot).
func TestSearchCodeDiversifyFilesCapsPerFileAndKeepsCount(t *testing.T) {
	root := t.TempDir()
	// big.go: six functions whose names carry all query terms -> highest scores.
	write(t, root, "big.go", `package p

func alphaBetaGammaOne()   { println("x") }
func alphaBetaGammaTwo()   { println("x") }
func alphaBetaGammaThree() { println("x") }
func alphaBetaGammaFour()  { println("x") }
func alphaBetaGammaFive()  { println("x") }
func alphaBetaGammaSix()   { println("x") }
`)
	// two smaller files, terms in a string literal -> real candidates, lower
	// score than the name matches in big.go.
	write(t, root, "small1.go", `package p

func helperOne() string { return "alpha beta gamma one" }
`)
	write(t, root, "small2.go", `package p

func helperTwo() string { return "alpha beta gamma two" }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	countByFile := func(hits []SearchCodeHit) map[string]int {
		m := map[string]int{}
		for _, h := range hits {
			m[h.File]++
		}
		return m
	}

	off := SearchCode(idx, "alpha beta gamma", SearchCodeOptions{Limit: 5, Mode: ModeEvidence})
	on := SearchCode(idx, "alpha beta gamma", SearchCodeOptions{Limit: 5, Mode: ModeEvidence, DiversifyFiles: true})
	if off.Status != "ok" || on.Status != "ok" {
		t.Fatalf("unexpected status off=%s on=%s", off.Status, on.Status)
	}

	offByFile := countByFile(off.Hits)
	onByFile := countByFile(on.Hits)

	// No under-fill: diversify returns the same number of hits as the control.
	if len(on.Hits) != len(off.Hits) {
		t.Fatalf("diversify changed hit count: off=%d(%v) on=%d(%v)", len(off.Hits), offByFile, len(on.Hits), onByFile)
	}
	// Per-file cap enforced.
	if onByFile["big.go"] > 3 {
		t.Fatalf("diversify should cap big.go at 3 hits, got %d: %v", onByFile["big.go"], onByFile)
	}
	// Diversity did not decrease, and the fixture is designed so it strictly
	// improves (big.go dominates without the cap).
	if len(onByFile) < len(offByFile) {
		t.Fatalf("diversify reduced file diversity: off=%v on=%v", offByFile, onByFile)
	}
	if offByFile["big.go"] > 3 && len(onByFile) <= len(offByFile) {
		t.Fatalf("expected diversify to add distinct files when big.go dominated: off=%v on=%v", offByFile, onByFile)
	}
}
