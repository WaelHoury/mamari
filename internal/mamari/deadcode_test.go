package mamari

import "testing"

// TestDeadCodeClassUsedOnlyViaInstanceMethodsIsNotFlagged covers a case where
// `new ClassName()` produces no edge into the class symbol itself — only
// `calls` edges into whatever methods get called on the resulting instance
// do. Before propagateDeadCodeUsageToParents, any class used entirely
// through its own instance methods (the common case) had zero inbound
// edges to the class symbol and was a false positive. Verified at
// real-repo scale: 61/107 classes (57%) in sv3-backend were false
// positives before this fix.
func TestDeadCodeClassUsedOnlyViaInstanceMethodsIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Logger.js", `class Logger {
  write(msg) {
    return msg
  }
}
module.exports = Logger
`)
	write(t, root, "user.js", `const Logger = require('./Logger')
const logger = new Logger()
function doWork() {
  logger.write('hi')
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "Logger") {
		t.Fatalf("expected Logger to NOT be flagged dead (used via new Logger() + logger.write()), got %#v", resp.Symbols)
	}
}

// TestDeadCodeClassUsedOnlyByConstructionIsNotFlagged covers classes whose
// only runtime use is construction (custom Error/value classes are common
// examples). A constructor call is real usage even when no instance method
// is subsequently called.
func TestDeadCodeClassUsedOnlyByConstructionIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "TooManyRequestError.js", `class TooManyRequestError extends Error {}
module.exports = TooManyRequestError
`)
	write(t, root, "handler.js", `const TooManyRequestError = require('./TooManyRequestError')
function rejectRequest() {
  throw new TooManyRequestError('slow down')
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "TooManyRequestError") {
		t.Fatalf("expected construct-only TooManyRequestError to NOT be flagged dead, got symbols=%#v edges=%#v", resp.Symbols, idx.snapshot().SymbolEdges)
	}
}

// TestDeadCodeRubyClassUsedOnlyByNewIsNotFlagged covers a precision-audit
// finding distinct from the JS `new`-construction fix above: Ruby has no
// `new` keyword at all — `Used.new(1)` is just an ordinary method call
// where the literal callee text happens to be "new" (Kernel-provided,
// never user-defined), unlike every other language this engine handles,
// where construction syntax's callee is the class name itself. The generic
// bare-name resolver therefore had no way to ever resolve a Ruby `.new`
// call, so any class only ever constructed this way had zero inbound
// edges and was a false positive — found directly via this exact case,
// not inferred from a benchmark.
func TestDeadCodeRubyClassUsedOnlyByNewIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "used.rb", `class Used
  def initialize(x)
    @x = x
  end
end
`)
	write(t, root, "main.rb", `def top
  u = Used.new(1)
end
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "Used") {
		t.Fatalf("expected Ruby construct-only Used to NOT be flagged dead, got symbols=%#v edges=%#v", resp.Symbols, idx.snapshot().SymbolEdges)
	}
}

// TestDeadCodeGenuinelyUnusedClassIsStillFlagged is the regression guard for
// the fix above: propagateDeadCodeUsageToParents must not blanket-exempt
// every class — a class with no usage anywhere (not instantiated, no
// methods called) must still be reported.
func TestDeadCodeGenuinelyUnusedClassIsStillFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Unused.js", `class UnusedHelper {
  run() {
    return 1
  }
}
module.exports = UnusedHelper
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if !isDeadCodeCandidate(resp, "UnusedHelper") {
		t.Fatalf("expected genuinely unused UnusedHelper to still be flagged dead, got %#v", resp.Symbols)
	}
}

// TestDeadCodeMethodUsageDoesNotFalselyClearUnrelatedClass covers a false
// negative the propagation fix must not introduce: Class B's method being
// called must not cause unrelated Class A (which shares no inheritance or
// other relationship with B) to be marked referenced. Propagation must only
// follow real ParentID links, never cross between unrelated symbols.
func TestDeadCodeMethodUsageDoesNotFalselyClearUnrelatedClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "classes.js", `class Used {
  run() {
    return 1
  }
}
class Unused {
  run() {
    return 2
  }
}
module.exports = { Used, Unused }
`)
	write(t, root, "user.js", `const { Used } = require('./classes')
const used = new Used()
function doWork() {
  used.run()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "Used") {
		t.Fatalf("expected Used to NOT be flagged dead, got %#v", resp.Symbols)
	}
	if !isDeadCodeCandidate(resp, "Unused") {
		t.Fatalf("expected Unused to still be flagged dead (propagation must not leak across unrelated classes), got %#v", resp.Symbols)
	}
}

func isDeadCodeCandidate(resp DeadCodeResponse, name string) bool {
	for _, s := range resp.Symbols {
		if s.Name == name {
			return true
		}
	}
	return false
}
