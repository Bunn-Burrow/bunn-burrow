package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bunn-Burrow/bunn-cli/internal/config"
	"github.com/Bunn-Burrow/bunn-cli/internal/output"
	"github.com/omkar/sandbox/agent"
)

var (
	agentManagerLock   sync.Mutex
	globalAgentManager *agent.Manager
)

func newAgentWriter(cmd *cobra.Command, loader *config.Loader) *output.Writer {
	format := output.Format(loader.Viper().GetString(config.KeyOutputFormat))
	if !format.Valid() {
		format = output.FormatTable
	}
	color := loader.Viper().GetString(config.KeyOutputColor)
	if color == "" {
		color = "auto"
	}
	w := output.NewWriter(format, color)
	if cmd != nil {
		w.SetOutput(cmd.OutOrStdout(), cmd.ErrOrStderr())
	}
	return w
}

// getAgentManager returns the session-scoped agent Manager.
func getAgentManager(loader *config.Loader) *agent.Manager {
	agentManagerLock.Lock()
	defer agentManagerLock.Unlock()

	if globalAgentManager == nil {
		vmmCfg, _ := buildVMMConfigFromViper(loader, "")
		provider := agent.NewRealVMProvider(vmmCfg)
		globalAgentManager = agent.NewManager(provider)
	}
	return globalAgentManager
}

// SetAgentManagerForTest allows injecting a mock or configured manager for unit testing.
func SetAgentManagerForTest(m *agent.Manager) {
	agentManagerLock.Lock()
	defer agentManagerLock.Unlock()
	globalAgentManager = m
}

// newAgentCommand creates the `agent` subcommand group.
func newAgentCommand(loader *config.Loader) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage AI agent executions",
		Long: `Create, start, inspect, list, cancel, and stream AI agent executions.

Bunn Burrow provides a secure, isolated microVM runtime for autonomous AI agents,
supporting Python, Go, Node.js, LangGraph, CrewAI, AutoGen, Agno, Claude Code,
OpenAI Codex, OpenCode, and custom agent binaries.`,
	}

	cmd.AddCommand(newAgentCreateCommand(loader))
	cmd.AddCommand(newAgentStartCommand(loader))
	cmd.AddCommand(newAgentRunCommand(loader))
	cmd.AddCommand(newAgentInspectCommand(loader))
	cmd.AddCommand(newAgentListCommand(loader))
	cmd.AddCommand(newAgentCancelCommand(loader))
	cmd.AddCommand(newAgentLogsCommand(loader))

	return cmd
}

// newAgentCreateCommand creates `bunn agent create`.
func newAgentCreateCommand(loader *config.Loader) *cobra.Command {
	var (
		id          string
		framework   string
		cmdStr      string
		args        []string
		envSlice    []string
		secretSlice []string
		vcpu        int64
		mem         int64
		timeout     time.Duration
		workdir     string
		templateRef string
		imageRef    string
		netEnabled  bool
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an agent execution",
		Long: `Create and configure a new agent execution in the Created state.
Secrets specified with --secret are securely injected at runtime and never persisted.`,
		Example: `  bunn agent create --id my-agent --framework python --cmd python3 --args main.py
  bunn agent create --framework langgraph --cmd python3 --secret OPENAI_API_KEY=sk-...`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr := getAgentManager(loader)
			w := newAgentWriter(cmd, loader)

			envMap := parseKeyValueSlice(envSlice)
			secretMap := parseKeyValueSlice(secretSlice)

			cfg := agent.AgentConfig{
				ID:          id,
				Framework:   agent.Framework(framework),
				Command:     cmdStr,
				Args:        args,
				Env:         envMap,
				Secrets:     secretMap,
				TemplateRef: templateRef,
				ImageRef:    imageRef,
				Timeout:     timeout,
				Resources: agent.ResourceConfig{
					VCPU:       vcpu,
					MemSizeMiB: mem,
				},
			}

			if workdir != "" {
				cfg.Workspace = &agent.WorkspaceConfig{
					WorkingDir: workdir,
				}
			}

			if netEnabled {
				cfg.Network = &agent.NetworkConfig{
					Enabled: true,
				}
			}

			exec, err := mgr.Create(cmd.Context(), cfg)
			if err != nil {
				return err
			}

			if loader.Viper().GetString(config.KeyOutputFormat) == "json" {
				return w.JSON(exec)
			}

			_ = w.Successf("Created agent execution %q\n", exec.ID)
			_ = w.Table([]string{"FIELD", "VALUE"}, [][]string{
				{"ID", exec.ID},
				{"Framework", string(exec.Framework)},
				{"Command", exec.Command},
				{"State", string(exec.State)},
			})

			return nil
		},
	}

	cmd.Flags().StringVar(&id, "id", "", "Unique execution ID (auto-generated if empty)")
	cmd.Flags().StringVar(&framework, "framework", "custom", "Agent framework (python, go, node, langgraph, crewai, claude-code, etc.)")
	cmd.Flags().StringVar(&cmdStr, "cmd", "", "Agent command executable")
	cmd.Flags().StringSliceVar(&args, "args", nil, "Command arguments")
	cmd.Flags().StringSliceVar(&envSlice, "env", nil, "Environment variables (KEY=VALUE)")
	cmd.Flags().StringSliceVar(&secretSlice, "secret", nil, "Sensitive credentials to inject securely (KEY=VALUE)")
	cmd.Flags().Int64Var(&vcpu, "vcpu", 2, "vCPU count")
	cmd.Flags().Int64Var(&mem, "mem", 1024, "Memory size in MiB")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Execution timeout (e.g. 30s, 5m)")
	cmd.Flags().StringVar(&workdir, "workdir", "/workspace", "Working directory in guest")
	cmd.Flags().StringVar(&templateRef, "template", "", "Template reference (W5)")
	cmd.Flags().StringVar(&imageRef, "image", "", "Image reference (W5)")
	cmd.Flags().BoolVar(&netEnabled, "net", false, "Enable VM network connectivity for tool/LLM access")

	return cmd
}

// newAgentStartCommand creates `bunn agent start <id>`.
func newAgentStartCommand(loader *config.Loader) *cobra.Command {
	return &cobra.Command{
		Use:   "start <execution-id>",
		Short: "Start an agent execution",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			mgr := getAgentManager(loader)
			w := newAgentWriter(cmd, loader)

			if err := mgr.Start(cmd.Context(), id); err != nil {
				return err
			}

			_ = w.Successf("Started agent execution %q\n", id)
			return nil
		},
	}
}

// newAgentRunCommand creates `bunn agent run` (shortcut for create + start + wait).
func newAgentRunCommand(loader *config.Loader) *cobra.Command {
	var (
		id          string
		framework   string
		cmdStr      string
		args        []string
		envSlice    []string
		secretSlice []string
		vcpu        int64
		mem         int64
		timeout     time.Duration
		workdir     string
		templateRef string
		imageRef    string
		netEnabled  bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Create and run an agent execution, streaming its output",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr := getAgentManager(loader)
			w := newAgentWriter(cmd, loader)

			envMap := parseKeyValueSlice(envSlice)
			secretMap := parseKeyValueSlice(secretSlice)

			cfg := agent.AgentConfig{
				ID:          id,
				Framework:   agent.Framework(framework),
				Command:     cmdStr,
				Args:        args,
				Env:         envMap,
				Secrets:     secretMap,
				TemplateRef: templateRef,
				ImageRef:    imageRef,
				Timeout:     timeout,
				Resources: agent.ResourceConfig{
					VCPU:       vcpu,
					MemSizeMiB: mem,
				},
			}

			if workdir != "" {
				cfg.Workspace = &agent.WorkspaceConfig{
					WorkingDir: workdir,
				}
			}

			if netEnabled {
				cfg.Network = &agent.NetworkConfig{
					Enabled: true,
				}
			}

			exec, err := mgr.Create(cmd.Context(), cfg)
			if err != nil {
				return err
			}

			// Subscribe to stream output
			ch, unsub, err := mgr.Subscribe(exec.ID)
			if err != nil {
				return err
			}
			defer unsub()

			if err := mgr.Start(cmd.Context(), exec.ID); err != nil {
				return err
			}

			// Stream output to terminal
			doneCh := make(chan *agent.ExecutionResult, 1)
			go func() {
				res, _ := mgr.Wait(cmd.Context(), exec.ID)
				doneCh <- res
			}()

			streamDone := false
			for !streamDone {
				select {
				case ev, ok := <-ch:
					if !ok {
						streamDone = true
						break
					}
					if ev.Type == agent.EventStdout {
						fmt.Print(ev.Data)
					} else if ev.Type == agent.EventStderr {
						fmt.Fprint(os.Stderr, ev.Data)
					} else if ev.Type == agent.EventFinished {
						streamDone = true
					}
				case <-doneCh:
					streamDone = true
				}
			}

			res, err := mgr.GetResult(exec.ID)
			if err != nil {
				return err
			}

			if res.State == agent.StateSucceeded {
				_ = w.Successf("Agent execution %q succeeded (exit code %d)\n", exec.ID, res.ExitCode)
				return nil
			}

			_ = w.Errorf("Agent execution %q %s: %s\n", exec.ID, res.State, res.Error)
			if res.ExitCode != 0 {
				os.Exit(res.ExitCode)
			}
			return fmt.Errorf("agent execution %s", res.State)
		},
	}

	cmd.Flags().StringVar(&id, "id", "", "Unique execution ID")
	cmd.Flags().StringVar(&framework, "framework", "custom", "Agent framework")
	cmd.Flags().StringVar(&cmdStr, "cmd", "", "Command executable")
	cmd.Flags().StringSliceVar(&args, "args", nil, "Command arguments")
	cmd.Flags().StringSliceVar(&envSlice, "env", nil, "Environment variables (KEY=VALUE)")
	cmd.Flags().StringSliceVar(&secretSlice, "secret", nil, "Sensitive credentials (KEY=VALUE)")
	cmd.Flags().Int64Var(&vcpu, "vcpu", 2, "vCPU count")
	cmd.Flags().Int64Var(&mem, "mem", 1024, "Memory size in MiB")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Execution timeout")
	cmd.Flags().StringVar(&workdir, "workdir", "/workspace", "Working directory in guest")
	cmd.Flags().StringVar(&templateRef, "template", "", "Template reference")
	cmd.Flags().StringVar(&imageRef, "image", "", "Image reference")
	cmd.Flags().BoolVar(&netEnabled, "net", false, "Enable VM networking")

	return cmd
}

// newAgentInspectCommand creates `bunn agent inspect <id>`.
func newAgentInspectCommand(loader *config.Loader) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <execution-id>",
		Short: "Inspect an agent execution details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			mgr := getAgentManager(loader)
			w := newAgentWriter(cmd, loader)

			exec, err := mgr.Inspect(id)
			if err != nil {
				return err
			}

			if loader.Viper().GetString(config.KeyOutputFormat) == "json" {
				return w.JSON(exec)
			}

			exitCodeStr := "-"
			if exec.ExitCode != nil {
				exitCodeStr = fmt.Sprintf("%d", *exec.ExitCode)
			}

			rows := [][]string{
				{"ID", exec.ID},
				{"Framework", string(exec.Framework)},
				{"State", string(exec.State)},
				{"VM ID", exec.VMID},
				{"Command", exec.Command},
				{"Args", strings.Join(exec.Args, " ")},
				{"Exit Code", exitCodeStr},
				{"Created At", exec.CreatedAt.Format(time.RFC3339)},
			}

			if exec.Error != "" {
				rows = append(rows, []string{"Error", exec.Error})
			}

			// Display sanitized environment variables
			if len(exec.Env) > 0 {
				var envEntries []string
				for k, v := range exec.Env {
					envEntries = append(envEntries, fmt.Sprintf("%s=%s", k, v))
				}
				rows = append(rows, []string{"Environment", strings.Join(envEntries, ", ")})
			}

			w.Table([]string{"FIELD", "VALUE"}, rows)
			return nil
		},
	}
}

// newAgentListCommand creates `bunn agent list`.
func newAgentListCommand(loader *config.Loader) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List agent executions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr := getAgentManager(loader)
			w := newAgentWriter(cmd, loader)

			list := mgr.List()

			if loader.Viper().GetString(config.KeyOutputFormat) == "json" {
				return w.JSON(list)
			}

			if len(list) == 0 {
				w.Infof("No agent executions found.\n")
				return nil
			}

			headers := []string{"ID", "FRAMEWORK", "STATE", "EXIT", "CREATED"}
			var rows [][]string
			for _, exec := range list {
				exitStr := "-"
				if exec.ExitCode != nil {
					exitStr = fmt.Sprintf("%d", *exec.ExitCode)
				}
				rows = append(rows, []string{
					exec.ID,
					string(exec.Framework),
					string(exec.State),
					exitStr,
					exec.CreatedAt.Format(time.RFC3339),
				})
			}

			w.Table(headers, rows)
			return nil
		},
	}
}

// newAgentCancelCommand creates `bunn agent cancel <id>`.
func newAgentCancelCommand(loader *config.Loader) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <execution-id>",
		Short: "Cancel an active agent execution",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			mgr := getAgentManager(loader)
			w := newAgentWriter(cmd, loader)

			if err := mgr.Cancel(cmd.Context(), id); err != nil {
				return err
			}

			_ = w.Successf("Cancelled agent execution %q\n", id)
			return nil
		},
	}
}

// newAgentLogsCommand creates `bunn agent logs <id>`.
func newAgentLogsCommand(loader *config.Loader) *cobra.Command {
	return &cobra.Command{
		Use:     "logs <execution-id>",
		Aliases: []string{"output"},
		Short:   "View logs and output of an agent execution",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			mgr := getAgentManager(loader)

			res, err := mgr.GetResult(id)
			if err != nil {
				return err
			}

			if res.Stdout != "" {
				fmt.Print(res.Stdout)
			}
			if res.Stderr != "" {
				fmt.Fprint(os.Stderr, res.Stderr)
			}
			return nil
		},
	}
}

func parseKeyValueSlice(slice []string) map[string]string {
	if len(slice) == 0 {
		return nil
	}
	res := make(map[string]string, len(slice))
	for _, item := range slice {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 {
			res[parts[0]] = parts[1]
		} else if len(parts) == 1 {
			res[parts[0]] = ""
		}
	}
	return res
}
