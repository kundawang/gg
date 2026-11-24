package text

import "os"

// readCtxTestFile reads a testdata file (test-only helper).
func readCtxTestFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
