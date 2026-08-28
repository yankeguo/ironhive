// Package agent is the agent running inside containers managed by the
// controller; it doubles as the container's init (PID 1).
package agent

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

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
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	go func() {
		for range sig {
			reap()
		}
	}()
	log.Println("pid 1: zombie reaper enabled")
}

// reap drains all pending zombies. SIGCHLD delivery does not queue, so a
// single notification may cover several dead children.
func reap() {
	for {
		pid, err := reapOne()
		if err == syscall.EINTR {
			continue
		}
		if pid > 0 {
			log.Printf("pid 1: reaped pid %d", pid)
			continue
		}
		return
	}
}

// reapOne waits on one dead child without blocking, returning its pid, or 0
// when there is nothing to reap (including ECHILD).
func reapOne() (int, error) {
	var ws syscall.WaitStatus
	pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
	if err != nil && err != syscall.EINTR {
		return 0, err
	}
	return pid, err
}
