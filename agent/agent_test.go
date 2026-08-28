package agent

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"syscall"
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

func TestChildManagerOwnsCommandUntilWaitCompletes(t *testing.T) {
	m := newChildManager()
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := m.start(cmd); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	m.mu.Lock()
	owner := m.managed[pid]
	m.mu.Unlock()
	if owner != cmd {
		t.Fatal("started command was not registered")
	}

	err := m.wait(cmd)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("wait error = %v, want exit code 7", err)
	}
	m.mu.Lock()
	owner = m.managed[pid]
	m.mu.Unlock()
	if owner != nil {
		t.Fatal("waited command remains registered")
	}
}

func TestChildManagerReapsOnlyUnmanagedChildren(t *testing.T) {
	managed := &exec.Cmd{}
	var waited []int
	m := &childManager{
		managed: map[int]*exec.Cmd{11: managed},
		wake:    make(chan struct{}, 1),
		listChildren: func(parent int) ([]int, error) {
			if parent != 1 {
				t.Fatalf("parent = %d, want 1", parent)
			}
			return []int{11, 22, 33}, nil
		},
		waitChild: func(pid int) (int, error) {
			waited = append(waited, pid)
			if pid == 33 {
				return 0, syscall.ECHILD
			}
			return pid, nil
		},
		waitAny: func() (int, error) {
			t.Fatal("unexpected fallback wait")
			return 0, nil
		},
	}
	if err := m.reap(1); err != nil {
		t.Fatal(err)
	}
	if want := []int{22, 33}; !reflect.DeepEqual(waited, want) {
		t.Fatalf("waited for %v, want %v", waited, want)
	}
}

func TestChildManagerFallbackNeverStealsManagedChild(t *testing.T) {
	waited := false
	m := &childManager{
		managed: map[int]*exec.Cmd{11: {}},
		wake:    make(chan struct{}, 1),
		listChildren: func(int) ([]int, error) {
			return nil, errors.New("proc unavailable")
		},
		waitChild: func(int) (int, error) {
			t.Fatal("unexpected targeted wait")
			return 0, nil
		},
		waitAny: func() (int, error) {
			waited = true
			return 0, nil
		},
	}
	if err := m.reap(1); err == nil {
		t.Fatal("reap succeeded without procfs while a child was managed")
	}
	if waited {
		t.Fatal("fallback wait ran while a child was managed")
	}
}

func TestProcParentPID(t *testing.T) {
	status := []byte("Name:\ttest\nState:\tZ (zombie)\nPPid:\t42\n")
	if got, err := procParentPID(status); err != nil || got != 42 {
		t.Fatalf("procParentPID = %d, %v; want 42, nil", got, err)
	}
	if _, err := procParentPID([]byte("Name:\ttest\n")); err == nil {
		t.Fatal("missing PPid did not fail")
	}
}

func TestDirectChildrenValidatesPIDNamespace(t *testing.T) {
	// directChildren reads procfs, which only exists on Linux.
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	if _, err := directChildren(os.Getpid()); err != nil {
		t.Fatalf("current procfs rejected: %v", err)
	}
	if _, err := directChildren(os.Getpid() + 1); err == nil {
		t.Fatal("mismatched procfs namespace was accepted")
	}
}
