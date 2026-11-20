//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
	"time"
)

func backgroundAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
func launcherSignals() []os.Signal               { return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} }

func lockRuntime(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}
