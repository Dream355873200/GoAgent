// Example: DevOps Bot
//
// This demonstrates how to build an AI-powered DevOps assistant
// using the GoAgent framework. The bot can check service status,
// view logs, deploy services, and restart them.
//
// Usage:
//
//	export ANTHROPIC_API_KEY=your-key
//	go run main.go
package main

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/Dream355873200/GoAgent"
)

func main() {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Println("Please set ANTHROPIC_API_KEY environment variable")
		os.Exit(1)
	}

	// Create the app — one line for the basics.
	app := goagent.New(
		goagent.WithProvider(goagent.Anthropic(apiKey)),
		goagent.WithSystemPrompt(
			"You are a DevOps assistant. You help users manage their services. "+
				"Use the available tools to check status, view logs, deploy, and restart services. "+
				"Always check status before deploying. Be concise in your responses.",
		),
		goagent.WithMaxTurns(20),
		goagent.WithMemoryDir("./memory"), // cross-session memory
	)

	// ─── Register tools ───
	// Each tool: a struct for input, a Permission level, a function. That's it.

	// 1. Check service status — ReadOnly, auto-allowed.
	app.Tool("check_status", goagent.ToolDef{
		Description: "Check the status of a service. Returns health info.",
		Input:       StatusInput{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true, // safe to run in parallel
		Execute: func(ctx goagent.Context, in StatusInput) (string, error) {
			ctx.Progress("checking " + in.Service + "...")
			return checkStatus(in.Service)
		},
	})

	// 2. View logs — ReadOnly, auto-allowed.
	app.Tool("get_logs", goagent.ToolDef{
		Description: "Get recent logs for a service.",
		Input:       LogsInput{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in LogsInput) (string, error) {
			return getLogs(in.Service, in.Lines)
		},
	})

	// 3. Deploy — Dangerous, always warns + confirms.
	app.Tool("deploy", goagent.ToolDef{
		Description: "Deploy a service to the specified environment. This is a production-impacting operation.",
		Input:       DeployInput{},
		Permission:  goagent.Dangerous,
		Execute: func(ctx goagent.Context, in DeployInput) (string, error) {
			ctx.Progress(fmt.Sprintf("deploying %s to %s...", in.Service, in.Env))
			return deploy(in.Service, in.Env, in.Version)
		},
	})

	// 4. Restart — RequireApproval, asks every time.
	app.Tool("restart_service", goagent.ToolDef{
		Description: "Restart a service. Brief downtime expected.",
		Input:       RestartInput{},
		Permission:  goagent.RequireApproval,
		Execute: func(ctx goagent.Context, in RestartInput) (string, error) {
			return restart(in.Service)
		},
	})

	// 5. List services — ReadOnly.
	app.Tool("list_services", goagent.ToolDef{
		Description: "List all available services.",
		Input:       struct{}{}, // no input needed
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in struct{}) (string, error) {
			return listServices()
		},
	})

	// ─── Run ───
	// One line to start the CLI REPL.
	// Could also be: app.RunHTTP(":8080") for an API server.
	app.RunCLI()
}

// ─── Input types ───
// Just Go structs with tags. The framework generates JSON Schema automatically.

type StatusInput struct {
	Service string `json:"service" desc:"Name of the service to check" required:"true"`
}

type LogsInput struct {
	Service string `json:"service" desc:"Name of the service" required:"true"`
	Lines   int    `json:"lines,omitempty" desc:"Number of log lines to return (default 20)"`
}

type DeployInput struct {
	Service string `json:"service" desc:"Service to deploy" required:"true"`
	Env     string `json:"env" desc:"Target environment" enum:"staging,production" required:"true"`
	Version string `json:"version,omitempty" desc:"Version to deploy, defaults to latest"`
}

type RestartInput struct {
	Service string `json:"service" desc:"Service to restart" required:"true"`
}

// ─── Mock implementations ───
// In a real app, these would call kubectl, AWS, etc.

var services = map[string]string{
	"user-service":    "healthy",
	"payment-service": "healthy",
	"api-gateway":     "degraded",
	"notification":    "healthy",
}

func checkStatus(service string) (string, error) {
	time.Sleep(100 * time.Millisecond) // simulate latency
	status, ok := services[service]
	if !ok {
		return "", fmt.Errorf("service %q not found", service)
	}
	uptime := rand.Intn(720) + 1
	return fmt.Sprintf("Service: %s\nStatus: %s\nUptime: %dh\nCPU: %.1f%%\nMemory: %dMB",
		service, status, uptime, rand.Float64()*100, rand.Intn(512)+64), nil
}

func getLogs(service string, lines int) (string, error) {
	if lines <= 0 {
		lines = 20
	}
	if _, ok := services[service]; !ok {
		return "", fmt.Errorf("service %q not found", service)
	}
	levels := []string{"INFO", "INFO", "INFO", "WARN", "ERROR"}
	msgs := []string{
		"request processed in 45ms",
		"connection pool: 8/20 active",
		"health check passed",
		"slow query detected: 1.2s",
		"failed to connect to cache, retrying",
		"request processed in 12ms",
		"user login successful",
		"rate limit approaching threshold",
	}
	var logLines []string
	for i := 0; i < lines && i < 20; i++ {
		ts := time.Now().Add(-time.Duration(lines-i) * time.Minute).Format("15:04:05")
		level := levels[rand.Intn(len(levels))]
		msg := msgs[rand.Intn(len(msgs))]
		logLines = append(logLines, fmt.Sprintf("[%s] %s %s: %s", ts, service, level, msg))
	}
	return strings.Join(logLines, "\n"), nil
}

func deploy(service, env, version string) (string, error) {
	if version == "" {
		version = "latest"
	}
	if _, ok := services[service]; !ok {
		return "", fmt.Errorf("service %q not found", service)
	}
	time.Sleep(500 * time.Millisecond) // simulate deploy
	return fmt.Sprintf("Successfully deployed %s@%s to %s\nRollout: 4/4 replicas ready\nTime: 12s",
		service, version, env), nil
}

func restart(service string) (string, error) {
	if _, ok := services[service]; !ok {
		return "", fmt.Errorf("service %q not found", service)
	}
	time.Sleep(300 * time.Millisecond) // simulate restart
	return fmt.Sprintf("Service %s restarted successfully\nDowntime: 2.3s\nNew PID: %d",
		service, rand.Intn(90000)+10000), nil
}

func listServices() (string, error) {
	var lines []string
	for name, status := range services {
		lines = append(lines, fmt.Sprintf("  %-20s %s", name, status))
	}
	return "Available services:\n" + strings.Join(lines, "\n"), nil
}
