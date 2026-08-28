// Package agent is the agent running inside containers managed by the
// controller; it doubles as the container's init (PID 1).
package agent

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// shellChildren separates children owned by exec.Cmd from orphaned
// descendants adopted by PID 1. A generic wait4(-1) reaper can steal a
// managed command's exit status before Cmd.Wait observes it.
var shellChildren = newChildManager()

var reapOnce sync.Once

// ReapZombies starts a SIGCHLD-driven reaper that waits on dead children,
// preventing zombie processes from accumulating when ironhive-agent runs
// as PID 1 inside a container: orphaned grandchildren are reparented to
// PID 1, and only PID 1 can reap them.
//
// It is a no-op unless the current process is PID 1 — outside a container
// there is always a real init doing the reaping.
func ReapZombies() {
	if os.Getpid() != 1 {
		return
	}
	reapOnce.Do(func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGCHLD)
		go func() {
			for {
				select {
				case <-sig:
				case <-shellChildren.wake:
				}
				if err := shellChildren.reap(os.Getpid()); err != nil {
					log.Println("pid 1: zombie reaper:", err)
				}
			}
		}()
		log.Println("pid 1: selective zombie reaper enabled")
	})
}

// childManager owns the boundary between commands waited by exec.Cmd and
// adopted orphans waited by the PID 1 reaper.
type childManager struct {
	mu      sync.Mutex
	managed map[int]*exec.Cmd
	wake    chan struct{}

	listChildren func(int) ([]int, error)
	waitChild    func(int) (int, error)
	waitAny      func() (int, error)
}

func newChildManager() *childManager {
	return &childManager{
		managed:      make(map[int]*exec.Cmd),
		wake:         make(chan struct{}, 1),
		listChildren: directChildren,
		waitChild:    waitChild,
		waitAny:      reapOne,
	}
}

// start holds the ownership lock across Start and registration. A command
// may exit immediately after fork; the reaper must not inspect it in that
// gap and mistake it for an orphan.
func (m *childManager) start(cmd *exec.Cmd) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	m.managed[cmd.Process.Pid] = cmd
	return nil
}

// wait leaves the command registered until Cmd.Wait has completed all pipe
// cleanup. The owner comparison protects a newer command if the kernel
// reuses the PID before this call removes the old registration.
func (m *childManager) wait(cmd *exec.Cmd) error {
	pid := cmd.Process.Pid
	err := cmd.Wait()
	m.mu.Lock()
	if m.managed[pid] == cmd {
		delete(m.managed, pid)
	}
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return err
}

// reap scans every direct child and waits only for unowned zombies. SIGCHLD
// delivery is edge-triggered and may be coalesced, so each pass considers
// all children rather than assuming one signal per process.
func (m *childManager) reap(parent int) error {
	pids, err := m.listChildren(parent)
	if err != nil {
		return m.reapAnyIfIdle(err)
	}
	for _, pid := range pids {
		m.mu.Lock()
		if m.managed[pid] != nil {
			m.mu.Unlock()
			continue
		}
		reaped, waitErr := m.waitChild(pid)
		m.mu.Unlock()
		switch {
		case waitErr == nil && reaped > 0:
			log.Printf("pid 1: reaped orphan pid %d", reaped)
		case waitErr == nil, errors.Is(waitErr, syscall.ECHILD):
		default:
			return fmt.Errorf("wait pid %d: %w", pid, waitErr)
		}
	}
	return nil
}

// reapAnyIfIdle is a conservative fallback for containers without a
// readable procfs. Waiting for any PID is safe only while no exec.Cmd owns
// a child; Start is excluded by the same lock.
func (m *childManager) reapAnyIfIdle(listErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.managed) != 0 {
		return fmt.Errorf("list direct children: %w", listErr)
	}
	for {
		pid, err := m.waitAny()
		switch {
		case err == syscall.EINTR:
			continue
		case err == nil && pid > 0:
			log.Printf("pid 1: reaped orphan pid %d", pid)
		case err == nil, errors.Is(err, syscall.ECHILD):
			return nil
		default:
			return fmt.Errorf("fallback wait: %w", err)
		}
	}
}

// directChildren reads PPid from procfs instead of using wait4(-1), which
// cannot exclude commands that belong to exec.Cmd.
func directChildren(parent int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == parent {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			// Processes can disappear between ReadDir and ReadFile.
			continue
		}
		ppid, err := procParentPID(data)
		if err == nil && ppid == parent {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

func procParentPID(status []byte) (int, error) {
	for _, line := range strings.Split(string(status), "\n") {
		if raw, ok := strings.CutPrefix(line, "PPid:"); ok {
			ppid, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return 0, err
			}
			return ppid, nil
		}
	}
	return 0, fmt.Errorf("PPid not found")
}

func waitChild(pid int) (int, error) {
	for {
		var ws syscall.WaitStatus
		reaped, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		return reaped, err
	}
}

// reapOne waits on any dead child without blocking. Production uses it only
// as the no-managed-child fallback when procfs cannot be inspected.
func reapOne() (int, error) {
	var ws syscall.WaitStatus
	pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
	if err != nil && err != syscall.EINTR {
		return 0, err
	}
	return pid, err
}
