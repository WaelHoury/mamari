package mamari

import "testing"

func TestIsTestPathRecognizesNestedCrossLanguageLayouts(t *testing.T) {
	tests := map[string]bool{
		"test/root.ts": true,
		"module/common/test/io/example/EngineTest.kt":       true,
		"src/test/java/example/ServiceTest.java":            true,
		"server/server-test-suites/Engine.kt":               true,
		"client/client-tests/Client.kt":                     true,
		"pkg/__tests__/router.spec.ts":                      true,
		"internal/testdata/generated.go":                    true,
		"src/unit/handler_test.py":                          true,
		"src/unit/test_handler.py":                          true,
		"src/main/java/example/Contest.java":                false,
		"src/main/kotlin/example/Latest.kt":                 false,
		"src/testing/production/Runtime.kt":                 false,
		"src/application/services/request_processor.py":     false,
		"server/common/src/io/example/server/TestEngine.kt": false,
	}
	for path, want := range tests {
		if got := isTestPath(path); got != want {
			t.Errorf("isTestPath(%q)=%t, want %t", path, got, want)
		}
	}
}
