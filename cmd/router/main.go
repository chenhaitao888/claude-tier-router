package main

import (
	"claude-tier-router/internal/router"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
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
		return errors.New("usage: router serve|claude|check|demo [-config config.json] [-- claude arguments]")
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	path := fs.String("config", "config.json", "configuration path")
	listen := fs.String("listen", "127.0.0.1:8787", "demo listen address")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if command == "demo" {
		return router.RunDemo(*listen)
	}
	if command != "serve" && command != "claude" && command != "check" {
		return errors.New("unknown command")
	}
	c, err := router.LoadConfig(*path)
	if err != nil {
		return err
	}
	if command == "check" {
		fmt.Println("Configuration valid; mode:", c.Mode)
		fmt.Printf("Local question summaries: %t (max %d characters).\n", c.StoreTaskSummaries, router.TaskSummaryChars)
		if c.High.Prices == nil || c.Low.Prices == nil {
			fmt.Println("Prices not fully configured: costs remain unknown.")
		}
		fmt.Println("Provider URLs and credentials have not been probed.")
		return nil
	}
	if err = c.CheckSecrets(); err != nil {
		return err
	}
	store, err := router.OpenStore(c.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	gateway := router.NewGateway(c, store)
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: gateway, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	address := "http://" + listener.Addr().String()
	fmt.Println("Local dashboard:", address, "| mode:", c.Mode)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if server.Shutdown(ctx) != nil {
			server.Close()
		}
	}()
	if command == "claude" {
		child := exec.Command("claude", fs.Args()...)
		child.Stdin = os.Stdin
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		child.Env = launchEnv(os.Environ(), c, address)
		return child.Run()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		return nil
	case err = <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
func launchEnv(existing []string, c router.Config, address string) []string {
	window := c.High.ContextTokens
	if c.Low.ContextTokens < window {
		window = c.Low.ContextTokens
	}
	replace := map[string]string{
		"ANTHROPIC_BASE_URL": address, "ANTHROPIC_AUTH_TOKEN": "local-router",
		"ANTHROPIC_MODEL": "router-auto", "ANTHROPIC_DEFAULT_OPUS_MODEL": "router-auto", "ANTHROPIC_DEFAULT_SONNET_MODEL": "router-auto", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "router-auto",
		"CLAUDE_CODE_SUBAGENT_MODEL": "router-auto", "CLAUDE_CODE_GATEWAY_HINT_HEADERS": "1", "CLAUDE_CODE_AUTO_COMPACT_WINDOW": strconv.Itoa(window * 3 / 4),
	}
	remove := map[string]bool{"ANTHROPIC_API_KEY": true, "CLAUDE_CODE_USE_BEDROCK": true, "CLAUDE_CODE_USE_VERTEX": true, "CLAUDE_CODE_USE_FOUNDRY": true, "CLAUDE_CODE_USE_MANTLE": true, "CLAUDE_CODE_USE_ANTHROPIC_AWS": true}
	env := []string{}
	for _, entry := range existing {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := replace[key]; !ok && !remove[key] {
			env = append(env, entry)
		}
	}
	for k, v := range replace {
		env = append(env, k+"="+v)
	}
	return env
}
