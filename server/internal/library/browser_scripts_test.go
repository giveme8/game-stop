package library

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The browser scripts (auth.js, connect.js, game-stop.js) are credential-handling
// surfaces, and they are the only code here that no Go test can execute. Run
// them through node's test runner when node is available -- CI's runner ships
// node, and so do the dev machines -- and say so out loud when it is missing,
// instead of quietly reporting a pass that proved nothing.
func TestBrowserScriptsWithNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		// CI must actually run these: a silent skip would look like a pass.
		if os.Getenv("CI") != "" {
			t.Fatalf("node is required in CI so the browser-script tests really run")
		}
		t.Skip("node is not installed; the *.test.cjs browser-script tests did not run")
	}
	// Name the test files explicitly: node's own globbing does not accept a bare
	// "." on Windows, and the shell never expands arguments for os/exec.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.test.cjs"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no browser-script tests found in %s: %v", dir, err)
	}
	out, err := exec.Command("node", append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test failed: %v\n%s", err, out)
	}
}
