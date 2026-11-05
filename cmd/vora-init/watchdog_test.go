package main

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// A sandbox at its memory limit can leave the agent unscheduled for longer than
// its grace while the runner keeps pinging. Time the agent was not running is
// not silence from the runner: on waking it must still answer, and only ticks
// it is awake for count towards the exit.
func TestWatchdogCountsOnlyTicksItWasAwakeFor(t *testing.T) {
	cmd, client := startAgentProcessWithGrace(t, 1, "2s")
	pid := cmd.Process.Pid

	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	// Let the watchdog take the tick that queued up during the stop first.
	time.Sleep(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Ping(ctx); err != nil {
		t.Fatalf("ping after a 5s stop with a 2s grace: %v: the watchdog counted the stop as silence", err)
	}

	// With the runner gone for real, the watchdog still ends the agent.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("agent exit: %v, want a clean exit", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("agent outlived a 2s grace by 6s with nobody pinging it")
	}
}
