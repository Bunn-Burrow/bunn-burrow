package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Bunn-Burrow/bunn-burrow/pkg/environment"
	"github.com/Bunn-Burrow/bunn-burrow/vmm"
	"github.com/Bunn-Burrow/bunn-cli/internal/cli/helpers"
	"github.com/Bunn-Burrow/bunn-cli/internal/config"
)

func newTerminalCommand(loader *config.Loader) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "terminal",
		Short: "Open an interactive terminal in a VM",
		Long: `Open an interactive guest shell inside a transient Firecracker VM.

This command is session-scoped: it creates a VM for the lifetime of the command,
starts an interactive shell inside the guest, and destroys the VM when the session
exits. This is the simplest safe terminal workflow available with the current
architecture; there is no persistent host-side VM registry or reconnectable daemon.`,
	}

	cmd.AddCommand(newTerminalRunCommand(loader))
	return cmd
}

func newTerminalRunCommand(loader *config.Loader) *cobra.Command {
	var (
		vmID       string
		shell      string
		workdir    string
		vmWorkDir  string
		rows       int
		cols       int
		vcpu       int64
		mem        int64
		bin        string
		kernel     string
		rootfs     string
		jsonOut    bool
		network    bool
		bridge     string
		cidr       string
		gateway    string
		dnsServers []string
		natIface   string
		firewall   bool
		mtu        int
		image      string
		guestAgent string
		secrets    []string
	)

	cmd := &cobra.Command{
		Use:   "run [vm-id]",
		Short: "Create a VM and attach to an interactive shell",
		Long: `Create a transient VM, start an interactive shell in the guest, and
attach the current local terminal to it.

When stdin is a TTY, the local terminal is switched to raw mode so interactive
applications (vim, htop, Python REPL, etc.) behave correctly. The command exits
when the guest shell exits or when the local terminal session is interrupted.`,
		Example: `  bunn terminal run --shell /bin/bash
  bunn terminal run my-vm --workdir /workspace`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			if len(args) > 0 {
				vmID = args[0]
			}

			loader.BindPFlag(config.KeyNetworkDNS, cmd.Flags().Lookup("network-dns"))
			cfg, err := helpers.LoadVMMConfig(loader, cmd.Flags(), "vm-workdir", vmID)
			if err != nil {
				return err
			}
			if vcpu != 0 {
				cfg.VCPU = vcpu
			}
			if mem != 0 {
				cfg.MemSizeMiB = mem
			}
			if kernel != "" {
				cfg.KernelImage = kernel
			}
			if rootfs != "" {
				cfg.RootFS = rootfs
			}
			if bin != "" {
				cfg.BinPath = bin
			}

			imageReference := image
			if imageReference == "" && cfg.RootFS == "" {
				imageReference = "bunn/ubuntu:24.04"
			}
			imageMode := imageReference != ""
			if cmd.Flags().Changed("image") && cmd.Flags().Changed("rootfs") && rootfs != "" {
				return fmt.Errorf("--image and --rootfs cannot be used together")
			}
			if imageMode {
				cfg.RootFS = ""
			}

			networkEnabled := cfg.Network != nil && cfg.Network.Enabled
			if cmd.Flags().Changed("network") {
				networkEnabled = network
			} else if imageMode && !terminalNetworkSettingConfigured(loader) {
				networkEnabled = true
			}
			if networkEnabled {
				cfg.Network, err = helpers.ResolveNetworkConfig(loader)
				if err != nil {
					return err
				}
			} else {
				cfg.Network = nil
			}

			terminalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
			defer stopSignals()

			if imageMode {
				agentPath := terminalGuestAgentPath(guestAgent)
				agentVersion, err := environment.GuestAgentVersion(agentPath)
				if err != nil {
					return err
				}
				preparer, err := environment.NewPreparer(environment.Config{
					CacheDir:   loader.Viper().GetString(config.KeyStorageCacheDir),
					GuestAgent: agentPath,
				})
				if err != nil {
					return fmt.Errorf("initialize environment preparation: %w", err)
				}

				fmt.Fprintf(cmd.ErrOrStderr(), "Preparing image %s...\n", imageReference)
				prepared, err := preparer.Prepare(terminalCtx, imageReference, agentVersion)
				if err != nil {
					return fmt.Errorf("prepare image %q: %w", imageReference, err)
				}
				if isFullstackImage(imageReference) {
					prepared, err = preparer.BuildFullstack(terminalCtx, prepared, cfg, cmd.ErrOrStderr())
					if err != nil {
						return fmt.Errorf("build fullstack profile: %w", err)
					}
				}
				cfg.RootFS = prepared.RootfsPath
			}

			rootfsCopy, cleanupRootfs, err := createTerminalRootfsCopy(
				terminalCtx,
				cfg.RootFS,
				loader.Viper().GetString(config.KeyStorageRootDir),
			)
			if err != nil {
				return fmt.Errorf("create writable terminal rootfs: %w", err)
			}
			cfg.RootFS = rootfsCopy
			defer func() {
				if err := cleanupRootfs(); err != nil {
					runErr = errors.Join(runErr, fmt.Errorf("remove writable terminal rootfs: %w", err))
				}
			}()

			fmt.Fprintln(cmd.ErrOrStderr(), "Connecting to sandbox...")
			sbox, err := vmm.NewSandbox(cfg)
			if err != nil {
				return fmt.Errorf("create sandbox: %w", err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := sbox.Close(ctx); err != nil {
					runErr = errors.Join(runErr, fmt.Errorf("close sandbox: %w", err))
				}
			}()

			if err := sbox.Start(terminalCtx); err != nil {
				return fmt.Errorf("start sandbox: %w", err)
			}

			shellArgs := resolveTerminalCommand(shell)
			terminalEnv, err := terminalEnvironment(secrets)
			if err != nil {
				return err
			}
			sessionCfg := vmm.SessionConfig{
				Cmd:  shellArgs[0],
				Args: shellArgs[1:],
				Env:  terminalEnv,
				Dir:  workdir,
			}
			if rows <= 0 || cols <= 0 {
				if width, height, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
					rows = height
					cols = width
				}
			}
			if rows <= 0 {
				rows = 24
			}
			if cols <= 0 {
				cols = 80
			}
			sessionCfg.Rows = uint16(rows)
			sessionCfg.Cols = uint16(cols)

			session, err := sbox.StartSession(terminalCtx, sessionCfg)
			if err != nil {
				return fmt.Errorf("start terminal session: %w", err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := session.Terminate(ctx); err != nil {
					runErr = errors.Join(runErr, fmt.Errorf("terminate terminal session: %w", err))
				}
			}()

			fmt.Fprintln(cmd.ErrOrStderr(), "Connected.")
			fmt.Fprintln(cmd.ErrOrStderr())
			if jsonOut {
				meta := map[string]any{"vm_id": cfg.ID, "state": "running", "shell": shellArgs[0], "args": shellArgs[1:]}
				enc := json.NewEncoder(cmd.OutOrStdout())
				if err := enc.Encode(meta); err != nil {
					return err
				}
			}

			input := cmd.InOrStdin()
			output := cmd.OutOrStdout()
			ttyFile, isFile := input.(*os.File)
			stdinTTY := isFile && term.IsTerminal(int(ttyFile.Fd()))
			if stdinTTY {
				oldState, err := term.MakeRaw(int(ttyFile.Fd()))
				if err != nil {
					return fmt.Errorf("enter raw mode: %w", err)
				}
				defer func() {
					if err := term.Restore(int(ttyFile.Fd()), oldState); err != nil {
						runErr = errors.Join(runErr, fmt.Errorf("restore local terminal: %w", err))
					}
				}()
			}

			resizeErrCh := make(chan error, 1)
			if stdinTTY {
				go func() {
					ch := make(chan os.Signal, 1)
					signal.Notify(ch, syscall.SIGWINCH)
					defer signal.Stop(ch)
					for {
						select {
						case <-terminalCtx.Done():
							return
						case <-ch:
							width, height, err := term.GetSize(int(ttyFile.Fd()))
							if err != nil {
								resizeErrCh <- fmt.Errorf("read local terminal size: %w", err)
								return
							}
							if err := session.Resize(uint16(height), uint16(width)); err != nil {
								resizeErrCh <- fmt.Errorf("resize guest terminal: %w", err)
								return
							}
						}
					}
				}()
			}

			outputDone := make(chan error, 1)
			go func() {
				for {
					data, err := session.ReceiveOutput(terminalCtx)
					if err != nil {
						outputDone <- err
						return
					}
					if _, writeErr := output.Write(data); writeErr != nil {
						outputDone <- writeErr
						return
					}
				}
			}()

			type inputResult struct {
				data []byte
				err  error
			}
			inputCh := make(chan inputResult)
			go func() {
				buf := make([]byte, 4096)
				for {
					n, readErr := input.Read(buf)
					if n > 0 {
						chunk := append([]byte(nil), buf[:n]...)
						select {
						case inputCh <- inputResult{data: chunk}:
						case <-terminalCtx.Done():
							return
						}
					}
					if readErr != nil {
						select {
						case inputCh <- inputResult{err: readErr}:
						case <-terminalCtx.Done():
						}
						return
					}
				}
			}()

			for {
				select {
				case err := <-outputDone:
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return nil
					}
					if err.Error() == "session finished" {
						return nil
					}
					return err
				case <-terminalCtx.Done():
					return nil
				case err := <-resizeErrCh:
					return err
				case inputResult := <-inputCh:
					if inputResult.err != nil {
						if errors.Is(inputResult.err, io.EOF) {
							if err := session.SendInput([]byte{4}); err != nil {
								return fmt.Errorf("send terminal EOF: %w", err)
							}
							continue
						}
						return inputResult.err
					}
					if len(inputResult.data) > 0 {
						if err := session.SendInput(inputResult.data); err != nil {
							return fmt.Errorf("send terminal input: %w", err)
						}
					}
				}
			}
		},
	}

	cmd.Flags().StringVar(&shell, "shell", "/bin/sh", "Shell to run in the guest (e.g. /bin/bash or /bin/sh)")
	cmd.Flags().StringVar(&workdir, "workdir", "/", "Working directory to use inside the guest")
	cmd.Flags().StringVar(&vmWorkDir, "vm-workdir", "", "Host directory for VM resources (defaults to ~/.local/share/bunn-burrow/vms)")
	cmd.Flags().IntVar(&rows, "rows", 0, "Initial terminal row count")
	cmd.Flags().IntVar(&cols, "cols", 0, "Initial terminal column count")
	cmd.Flags().StringVar(&kernel, "kernel", "", "Kernel image path")
	cmd.Flags().StringVar(&rootfs, "rootfs", "", "Root filesystem image path")
	cmd.Flags().StringVar(&image, "image", "", "OCI image or Bunn preset (default: bunn/ubuntu:24.04 when no rootfs is configured)")
	cmd.Flags().StringVar(&guestAgent, "guest-agent", "", "Guest Agent binary path (defaults to BUNN_GUEST_AGENT or bin/guest-agent-static)")
	cmd.Flags().StringArrayVar(&secrets, "secret", nil, "Forward a host environment variable by name to the guest (repeatable; never pass secret values as arguments)")
	cmd.Flags().StringVar(&bin, "bin", "", "Firecracker binary path")
	cmd.Flags().Int64Var(&vcpu, "vcpu", 0, "Guest vCPU count")
	cmd.Flags().Int64Var(&mem, "mem", 0, "Guest memory in MiB")
	cmd.Flags().BoolVar(&network, "network", false, "Enable VM networking")
	cmd.Flags().StringVar(&bridge, "network-bridge", "", "Bridge name for VM TAPs")
	cmd.Flags().StringVar(&cidr, "network-cidr", "", "CIDR pool for VM IP allocation")
	cmd.Flags().StringVar(&gateway, "network-gateway", "", "Gateway IP for VM subnet")
	cmd.Flags().StringSliceVar(&dnsServers, "network-dns", nil, "Guest DNS server IPv4 address (repeatable)")
	cmd.Flags().StringVar(&natIface, "network-nat-iface", "", "Host interface for outbound masquerade")
	cmd.Flags().BoolVar(&firewall, "network-firewall", true, "Enable VM firewall")
	cmd.Flags().IntVar(&mtu, "network-mtu", 1500, "MTU for VM TAP devices")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output JSON metadata before attaching")
	return cmd
}

func resolveTerminalCommand(shell string) []string {
	trimmed := strings.TrimSpace(shell)
	if trimmed == "" {
		trimmed = "/bin/sh"
	}
	if strings.ContainsAny(trimmed, " \t\n\r\"'") {
		return splitCommandLine(trimmed)
	}
	return []string{trimmed, "-i"}
}

func splitCommandLine(s string) []string {
	var (
		parts []string
		cur   strings.Builder
		inS   bool
		inD   bool
	)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case ' ', '\t', '\n', '\r':
			if inS || inD {
				cur.WriteByte(ch)
				continue
			}
			if cur.Len() > 0 {
				parts = append(parts, cur.String())
				cur.Reset()
			}
		case '\\':
			if i+1 < len(s) && (inD || inS) {
				cur.WriteByte(s[i+1])
				i++
				continue
			}
			cur.WriteByte(ch)
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	if len(parts) == 0 {
		return []string{strings.TrimSpace(s)}
	}
	return parts
}

func terminalEnvironment(secretNames []string) ([]string, error) {
	terminal := os.Getenv("TERM")
	if terminal == "" {
		terminal = "xterm-256color"
	}
	env := []string{
		"HOME=/root",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"SHELL=/bin/sh",
		"TERM=" + terminal,
	}
	seen := map[string]struct{}{
		"HOME":  {},
		"PATH":  {},
		"SHELL": {},
		"TERM":  {},
	}
	for _, name := range secretNames {
		if !validEnvironmentName(name) {
			return nil, fmt.Errorf("invalid secret environment variable name %q", name)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate or reserved secret environment variable %q", name)
		}
		value, exists := os.LookupEnv(name)
		if !exists {
			return nil, fmt.Errorf("secret environment variable %q is not set on the host", name)
		}
		seen[name] = struct{}{}
		env = append(env, name+"="+value)
	}
	return env, nil
}

func validEnvironmentName(name string) bool {
	if name == "" || !(name[0] == '_' || name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z') {
		return false
	}
	for i := 1; i < len(name); i++ {
		ch := name[i]
		if ch != '_' && !(ch >= 'A' && ch <= 'Z') && !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') {
			return false
		}
	}
	return true
}

func terminalNetworkSettingConfigured(loader *config.Loader) bool {
	if loader.Viper().InConfig(config.KeyNetworkEnabled) {
		return true
	}
	_, exists := os.LookupEnv(config.EnvPrefix + "_NETWORK_ENABLED")
	return exists
}

func isFullstackImage(reference string) bool {
	return reference == "bunn/fullstack" || reference == "bunn/fullstack:latest"
}

func terminalGuestAgentPath(configured string) string {
	if configured != "" {
		return configured
	}
	if path := os.Getenv("BUNN_GUEST_AGENT"); path != "" {
		return path
	}
	candidates := []string{filepath.Join("bin", "guest-agent-static"), filepath.Join("..", "bin", "guest-agent-static")}
	if executable, err := os.Executable(); err == nil {
		candidates = append([]string{filepath.Join(filepath.Dir(executable), "guest-agent-static")}, candidates...)
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return candidates[0]
}

func createTerminalRootfsCopy(ctx context.Context, source, storageRoot string) (string, func() error, error) {
	if source == "" {
		return "", nil, fmt.Errorf("root filesystem path is required")
	}
	if storageRoot == "" {
		return "", nil, fmt.Errorf("storage root directory is required")
	}
	terminalRoot := filepath.Join(storageRoot, "terminals")
	if err := os.MkdirAll(terminalRoot, 0o700); err != nil {
		return "", nil, fmt.Errorf("create terminal storage directory: %w", err)
	}
	sessionDir, err := os.MkdirTemp(terminalRoot, "rootfs-")
	if err != nil {
		return "", nil, fmt.Errorf("create terminal rootfs directory: %w", err)
	}
	rootfsPath := filepath.Join(sessionDir, "rootfs.ext4")
	copyCmd := exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", "--preserve=mode", "--", source, rootfsPath)
	if output, err := copyCmd.CombinedOutput(); err != nil {
		removeErr := os.RemoveAll(sessionDir)
		return "", nil, errors.Join(fmt.Errorf("copy rootfs: %w: %s", err, string(output)), removeErr)
	}
	return rootfsPath, func() error {
		return os.RemoveAll(sessionDir)
	}, nil
}
