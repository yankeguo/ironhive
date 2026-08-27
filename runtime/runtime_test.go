package runtime

import (
	"os/exec"
	"testing"
	"time"
)

func TestReapOne(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The child exits quickly but stays a zombie until waited on; poll the
	// non-blocking reaper until it collects exactly our child.
	deadline := time.Now().Add(5 * time.Second)
	for {
		pid, _ := reapOne()
		if pid == cmd.Process.Pid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d was not reaped", cmd.Process.Pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
