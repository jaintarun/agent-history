package httpapi

import (
	"os/exec"
	"testing"
)

func TestBrowserRefreshBehavior(t *testing.T) {
	command := exec.Command("node", "testdata/app_behavior_test.js", "assets/app.js")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser behavior test failed: %v\n%s", err, output)
	}
}
