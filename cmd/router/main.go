package main

import (
	"claude-tier-router/internal/router"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: router claude|serve|status|stop|restart|check|demo [-config config.json] [-- claude arguments]")
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	path := fs.String("config", "", "configuration path (default: router checkout or user configuration)")
	listen := fs.String("listen", "127.0.0.1:8787", "demo listen address")
	force := fs.Bool("force", false, "stop/restart even if Claude sessions are active")
	cooperate := fs.Bool("cooperate", false, "Claude: let Opus plan/review and Flash execute bounded subtasks")
	sharedChild := fs.Bool("shared-child", false, "internal shared gateway startup")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if command == "demo" {
		return router.RunDemo(*listen)
	}
	if command != "serve" && command != "claude" && command != "check" && command != "status" && command != "stop" && command != "restart" {
		return errors.New("unknown command")
	}
	if *force && command != "stop" && command != "restart" {
		return errors.New("-force is only valid for stop and restart")
	}
	configPath, err := resolveConfigPath(*path)
	if err != nil {
		return err
	}
	c, err := router.LoadConfig(configPath)
	if err != nil {
		return err
	}
	launchConfig := c
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "cooperate" {
			launchConfig.Cooperation.Enabled = *cooperate
		}
	})
	if command == "claude" && launchConfig.Cooperation.Enabled && c.Mode != "auto" {
		return errors.New("cooperative Claude sessions require mode auto in Router config")
	}
	fmt.Println("Configuration:", configPath)
	if command == "check" {
		fmt.Println("Configuration valid; mode:", c.Mode)
		fmt.Printf("Local question summaries: %t (max %d characters).\n", c.StoreTaskSummaries, router.TaskSummaryChars)
		if c.High.Prices == nil || c.Low.Prices == nil {
			fmt.Println("Prices not fully configured: costs remain unknown.")
		}
		fmt.Println("Provider URLs and credentials have not been probed.")
		return nil
	}
	if command != "status" && command != "stop" {
		if err = c.CheckSecrets(); err != nil {
			return err
		}
	}
	if command == "status" || command == "stop" || command == "restart" {
		address, info, err := findRuntime(c)
		if errors.Is(err, errOtherService) {
			return err
		}
		if command == "status" {
			if err != nil {
				fmt.Println("No reachable shared Router at the configured address.")
				return nil
			}
			fmt.Printf("Router running: %s | PID: %d | active Claude sessions: %d | in-flight requests: %d | run: %s\n", address, info.PID, info.ActiveClients, info.ActiveRequests, info.RunID)
			return nil
		}
		if err == nil {
			if err := stopRuntime(address, info.RunID, *force); err != nil {
				return err
			}
		}
		if command == "stop" {
			fmt.Println("Shared Router stopped (or was already unavailable).")
			return nil
		}
	}
	if command == "serve" {
		return serveRuntime(c, *sharedChild)
	}
	if command == "claude" {
		if _, err := exec.LookPath("claude"); err != nil {
			return errors.New("Claude Code executable was not found on PATH")
		}
	}
	lease, err := ensureRuntime(c, configPath)
	if err != nil {
		return err
	}
	defer lease.Close()
	fmt.Println("Shared dashboard:", lease.Address, "| mode:", c.Mode)
	if command == "restart" {
		fmt.Println("Shared Router ready.")
		return nil
	}
	args, cleanup, err := launchArgs(fs.Args(), launchConfig, lease.Address)
	if err != nil {
		return err
	}
	defer cleanup()
	fmt.Println("Claude Code gateway:", lease.Address, "| shared service; session settings applied")
	if launchConfig.Cooperation.Enabled {
		fmt.Printf("Cooperation: %s for analysis/review, %s for execution and eligible delivery steps.\n", c.High.Model, c.Low.Model)
	}
	child := exec.Command("claude", args...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Env = launchEnv(os.Environ(), launchConfig, lease.Address)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, launcherSignals()...)
	defer signal.Stop(signals)
	if err := child.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	for {
		select {
		case err := <-done:
			return err
		case sig := <-signals:
			// Terminal Ctrl+C already reaches Claude in the same foreground group.
			// Keep the wrapper (and its lease) alive while Claude handles it.
			if sig == os.Interrupt {
				continue
			}
			child.Process.Signal(sig)
			select {
			case err := <-done:
				return err
			case <-time.After(5 * time.Second):
				child.Process.Kill()
				return <-done
			}
		}
	}
}
