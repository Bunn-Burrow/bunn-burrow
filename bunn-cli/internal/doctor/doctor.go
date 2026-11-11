// Package doctor provides runtime health checks for the bunn CLI.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Check is a single diagnostic check result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Result is the overall doctor result.
type Result struct {
	AllPassed bool    `json:"all_passed"`
	Checks    []Check `json:"checks"`
}

// Runner performs diagnostic checks.
type Runner struct{}

// NewRunner creates a new doctor runner.
func NewRunner() *Runner {
	return &Runner{}
}

// Run performs all diagnostic checks and returns the result.
func (r *Runner) Run(ctx context.Context) *Result {
	checks := runChecks(ctx)

	allPassed := true
	for _, c := range checks {
		if c.Status == "fail" {
			allPassed = false
		}
	}

	return &Result{
		AllPassed: allPassed,
		Checks:    checks,
	}
}

func runChecks(ctx context.Context) []Check {
	var checks []Check

	checks = append(checks, checkFirecracker(ctx))
	checks = append(checks, checkKVM(ctx))
	checks = append(checks, checkQemuImg(ctx))
	checks = append(checks, checkQemuNBD(ctx))
	checks = append(checks, checkOS(ctx))

	return checks
}

func checkFirecracker(ctx context.Context) Check {
	bin := "firecracker"
	if env := os.Getenv("FIRECRACKER_BIN"); env != "" {
		bin = env
	}

	path, err := exec.LookPath(bin)
	if err != nil {
		return Check{
			Name:   "firecracker",
			Status: "fail",
			Detail: fmt.Sprintf("Firecracker binary %q not found in PATH", bin),
		}
	}

	versionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(versionCtx, path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Check{
			Name:   "firecracker",
			Status: "warn",
			Detail: fmt.Sprintf("binary found at %s but --version failed: %v", path, err),
		}
	}
	version := strings.TrimSpace(string(out))

	return Check{
		Name:   "firecracker",
		Status: "pass",
		Detail: fmt.Sprintf("binary at %s (%s)", path, version),
	}
}

func checkKVM(ctx context.Context) Check {
	_, err := os.Stat("/dev/kvm")
	if err != nil {
		if os.IsNotExist(err) {
			return Check{
				Name:   "kvm",
				Status: "fail",
				Detail: "/dev/kvm does not exist; KVM acceleration unavailable",
			}
		}
		return Check{
			Name:   "kvm",
			Status: "fail",
			Detail: fmt.Sprintf("cannot stat /dev/kvm: %v", err),
		}
	}

	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return Check{
			Name:   "kvm",
			Status: "fail",
			Detail: fmt.Sprintf("/dev/kvm exists but is not readable/writable: %v", err),
		}
	}
	f.Close()

	return Check{
		Name:   "kvm",
		Status: "pass",
		Detail: "/dev/kvm available with read/write access",
	}
}

func checkQemuImg(ctx context.Context) Check {
	_, err := exec.LookPath("qemu-img")
	if err != nil {
		return Check{
			Name:   "qemu-img",
			Status: "fail",
			Detail: "qemu-img not found in PATH (required for qcow2 overlays)",
		}
	}
	return Check{
		Name:   "qemu-img",
		Status: "pass",
		Detail: "qemu-img available in PATH",
	}
}

func checkQemuNBD(ctx context.Context) Check {
	_, err := exec.LookPath("qemu-nbd")
	if err != nil {
		return Check{
			Name:   "qemu-nbd",
			Status: "warn",
			Detail: "qemu-nbd not found in PATH (required for template-based VMs)",
		}
	}
	return Check{
		Name:   "qemu-nbd",
		Status: "pass",
		Detail: "qemu-nbd available in PATH",
	}
}

func checkOS(ctx context.Context) Check {
	if runtime.GOOS != "linux" {
		return Check{
			Name:   "os",
			Status: "fail",
			Detail: fmt.Sprintf("host OS is %s; Firecracker requires Linux", runtime.GOOS),
		}
	}
	return Check{
		Name:   "os",
		Status: "pass",
		Detail: "host OS is Linux",
	}
}
