package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/Bunn-Burrow/bunn-burrow/guest-agent/internal/guestagent"
)

func main() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	var (
		entrypoint = flag.String("entrypoint", "", "OCI entrypoint (comma-separated)")
		cmd        = flag.String("cmd", "", "OCI cmd (comma-separated)")
		env        = flag.String("env", "", "OCI env (comma-separated KEY=VAL)")
		workdir    = flag.String("workdir", "", "OCI working directory")
		user       = flag.String("user", "", "OCI user")
	)
	flag.Parse()

	// Mount essential filesystems before running as PID 1.
	for _, m := range [][3]string{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
	} {
		target := m[1]
		if err := os.MkdirAll(target, 0o755); err != nil {
			log.Printf("mkdir %s: %v", target, err)
		}
		if err := syscall.Mount(m[0], target, m[2], 0, ""); err != nil {
			log.Printf("mount %s on %s: %v", m[0], target, err)
		}
	}
	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, "mode=0755"); err != nil && !errors.Is(err, syscall.EBUSY) {
		log.Printf("mount devtmpfs on /dev: %v", err)
	}
	if err := os.MkdirAll("/dev/pts", 0o755); err != nil {
		log.Printf("mkdir /dev/pts: %v", err)
	}
	if err := syscall.Mount("devpts", "/dev/pts", "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "gid=5,mode=620,ptmxmode=666"); err != nil {
		log.Printf("mount devpts on /dev/pts: %v", err)
	}
	if err := guestagent.ConfigureNetworkFromCmdline("/proc/cmdline"); err != nil {
		log.Fatalf("configure guest network: %v", err)
	}

	agent := guestagent.New()

	if err := agent.ValidatePID1(); err != nil {
		log.Fatalf("PID 1 validation failed: %v", err)
	}

	// Create process manager.
	pm := guestagent.NewProcessManager()
	agent.SetProcessManager(pm)

	// Create session manager for PTY sessions.
	sm := guestagent.NewSessionManager()

	// Build OCI config from flags.
	oci := &guestagent.OCIConfig{}
	if *entrypoint != "" {
		oci.Entrypoint = splitComma(*entrypoint)
	}
	if *cmd != "" {
		oci.Cmd = splitComma(*cmd)
	}
	if *env != "" {
		oci.Env = splitComma(*env)
	}
	oci.Workdir = *workdir
	oci.User = *user

	signals, stopSignals := guestagent.NotifySignals()
	defer stopSignals()

	// Context cancelled when vsock shutdown request is received.
	vsockCtx, vsockCancel := context.WithCancel(context.Background())

	// Set shutdown callback on agent.
	agent.SetShutdownFn(func() {
		log.Println("agent shutdown initiated")
	})

	// Start vsock server for health, shutdown, execution, and file transfer requests.
	vsockServer := guestagent.NewVSockServer(8080, func() {
		log.Println("vsock shutdown requested")
		vsockCancel()
	}, pm, sm, oci, guestagent.DefaultWorkspaceLimits())
	if err := vsockServer.Listen(); err != nil {
		log.Fatalf("vsock listen failed: %v", err)
	}
	go func() {
		if err := vsockServer.Serve(); err != nil {
			log.Printf("vsock server stopped: %v", err)
		}
	}()
	defer vsockServer.Close()

	log.Println("guest agent starting")

	if err := agent.Run(
		vsockCtx,
		signals,
		guestagent.ReapChildren,
	); err != nil {
		log.Printf("guest agent lifecycle error: %v", err)
	}

	log.Printf("guest agent state: %s", agent.State())
	log.Println("requesting Linux power-off")

	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		log.Printf("power-off failed: %v", err)
	}

	// PID 1 must not exit. If power-off fails, remain alive
	// so the kernel does not panic due to init exiting.
	for {
		time.Sleep(time.Hour)
	}
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, part := range split(s, ",") {
		if trimmed := trimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func split(s, sep string) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i:i+1] == sep {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	result = append(result, s[start:])
	return result
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && s[start] == ' ' {
		start++
	}
	for end > start && s[end-1] == ' ' {
		end--
	}
	return s[start:end]
}
