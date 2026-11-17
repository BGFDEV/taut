package devstack_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The load test's script checks its own arithmetic (percentiles from
// histograms, the theoretical bound, rates, the report) without a stack.
// Running that here keeps the script from going stale between load tests,
// which are run by hand.
func TestLoadTestScript(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	script := filepath.Join("..", "..", "scripts", "loadtest.py")
	out, err := exec.Command(python, script, "--self-test").CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/loadtest.py --self-test: %v\n%s", err, out)
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		if out, err := exec.Command(bash, "-n", filepath.Join("..", "..", "scripts", "load-test.sh")).CombinedOutput(); err != nil {
			t.Errorf("scripts/load-test.sh does not parse: %v\n%s", err, out)
		}
	}
}
