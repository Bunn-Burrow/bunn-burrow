package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/omkar/sandbox/vmm"
)

// VMSandbox defines the low-level microVM execution interface required by the agent orchestrator.
// This interface abstracts away the Firecracker process and vsock details, allowing
// testing with mocks/fakes without requiring hardware virtualization or Firecracker binaries.
type VMSandbox interface {
	ID() string
	Start(ctx context.Context) error
	Execute(ctx context.Context, w vmm.Workload, stdout, stderr io.Writer) (int, error)
	CancelExecution(ctx context.Context, execID string) error
	UploadProject(ctx context.Context, p *vmm.Project) error
	DownloadFile(ctx context.Context, projectID, path string, w io.Writer) error
	Close(ctx context.Context) error
}

// VMProvider abstracts the creation and provisioning of VMSandbox instances.
type VMProvider interface {
	CreateSandbox(ctx context.Context, cfg *AgentConfig) (VMSandbox, error)
}

// RealVMSandbox wraps a vmm.Sandbox to implement the VMSandbox interface.
type RealVMSandbox struct {
	sandbox *vmm.Sandbox
	id      string
}

func (r *RealVMSandbox) ID() string {
	if r.sandbox != nil && r.sandbox.VM() != nil {
		return r.sandbox.VM().ID()
	}
	return r.id
}

func (r *RealVMSandbox) Start(ctx context.Context) error {
	return r.sandbox.Start(ctx)
}

func (r *RealVMSandbox) Execute(ctx context.Context, w vmm.Workload, stdout, stderr io.Writer) (int, error) {
	return r.sandbox.Execute(ctx, w, stdout, stderr)
}

func (r *RealVMSandbox) CancelExecution(ctx context.Context, execID string) error {
	return r.sandbox.CancelExecution(ctx, execID)
}

func (r *RealVMSandbox) UploadProject(ctx context.Context, p *vmm.Project) error {
	return r.sandbox.UploadProject(ctx, p)
}

func (r *RealVMSandbox) DownloadFile(ctx context.Context, projectID, path string, w io.Writer) error {
	return r.sandbox.DownloadFile(ctx, projectID, path, w)
}

func (r *RealVMSandbox) Close(ctx context.Context) error {
	return r.sandbox.Close(ctx)
}

// RealVMProvider creates real Firecracker-backed sandboxes using the vmm package.
type RealVMProvider struct {
	BaseConfig vmm.Config
}

// NewRealVMProvider returns a RealVMProvider with the provided base configuration.
func NewRealVMProvider(baseCfg vmm.Config) *RealVMProvider {
	return &RealVMProvider{BaseConfig: baseCfg}
}

// CreateSandbox translates AgentConfig into a vmm.Config and boots a new Sandbox.
func (p *RealVMProvider) CreateSandbox(ctx context.Context, cfg *AgentConfig) (VMSandbox, error) {
	vmmCfg := p.BaseConfig
	if cfg.VMID != "" {
		vmmCfg.ID = cfg.VMID
	}
	if cfg.TemplateRef != "" {
		vmmCfg.TemplateID = cfg.TemplateRef
	}
	if cfg.ImageRef != "" {
		vmmCfg.ImageID = cfg.ImageRef
	}
	if cfg.Resources.VCPU > 0 {
		vmmCfg.VCPU = cfg.Resources.VCPU
	}
	if cfg.Resources.MemSizeMiB > 0 {
		vmmCfg.MemSizeMiB = cfg.Resources.MemSizeMiB
	}

	// Apply Network if configured
	if cfg.Network != nil && cfg.Network.Enabled {
		vmmCfg.Network = &vmm.NetworkConfig{
			Enabled:      true,
			Bridge:       cfg.Network.Bridge,
			CIDR:         cfg.Network.CIDR,
			NATInterface: cfg.Network.NATInterface,
			Firewall:     true,
		}
		if vmmCfg.Network.Bridge == "" {
			vmmCfg.Network.Bridge = "br-bunn"
		}
		if vmmCfg.Network.CIDR == "" {
			vmmCfg.Network.CIDR = "10.11.0.0/16"
		}
		if _, ipNet, err := net.ParseCIDR(vmmCfg.Network.CIDR); err == nil && ipNet != nil {
			// Derive gateway (first IP)
			gw := make(net.IP, len(ipNet.IP))
			copy(gw, ipNet.IP)
			gw[len(gw)-1]++
			vmmCfg.Network.Gateway = gw
		}
	}

	sb, err := vmm.NewSandbox(vmmCfg)
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}

	return &RealVMSandbox{sandbox: sb, id: vmmCfg.ID}, nil
}

// --- Fakes for Unit Testing ---

// FakeVMSandbox is an in-memory test double for VMSandbox.
type FakeVMSandbox struct {
	mu               sync.Mutex
	id               string
	StartErr         error
	ExecuteErr       error
	ExecuteCode      int
	ExecuteStdout    string
	ExecuteStderr    string
	ExecuteDelay     time.Duration
	CloseErr         error
	CancelErr        error
	UploadErr        error
	DownloadErr      error
	DownloadedData   []byte
	Closed           bool
	Cancelled        bool
	LastWorkload     vmm.Workload
	UploadedProj     *vmm.Project
	DisconnectOnExec bool
}

func NewFakeVMSandbox(id string) *FakeVMSandbox {
	return &FakeVMSandbox{
		id:          id,
		ExecuteCode: 0,
	}
}

func (f *FakeVMSandbox) ID() string {
	return f.id
}

func (f *FakeVMSandbox) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StartErr != nil {
		return f.StartErr
	}
	return nil
}

func (f *FakeVMSandbox) Execute(ctx context.Context, w vmm.Workload, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.LastWorkload = w
	delay := f.ExecuteDelay
	disconnect := f.DisconnectOnExec
	stdoutData := f.ExecuteStdout
	stderrData := f.ExecuteStderr
	execCode := f.ExecuteCode
	execErr := f.ExecuteErr
	f.mu.Unlock()

	if disconnect {
		return -1, io.ErrUnexpectedEOF
	}

	if delay > 0 {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(delay):
		}
	} else if ctx.Err() != nil {
		return -1, ctx.Err()
	}

	if stdout != nil && stdoutData != "" {
		_, _ = stdout.Write([]byte(stdoutData))
	}
	if stderr != nil && stderrData != "" {
		_, _ = stderr.Write([]byte(stderrData))
	}

	if execErr != nil {
		return execCode, execErr
	}
	return execCode, nil
}

func (f *FakeVMSandbox) CancelExecution(ctx context.Context, execID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Cancelled = true
	return f.CancelErr
}

func (f *FakeVMSandbox) UploadProject(ctx context.Context, p *vmm.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.UploadedProj = p
	return f.UploadErr
}

func (f *FakeVMSandbox) DownloadFile(ctx context.Context, projectID, path string, w io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DownloadErr != nil {
		return f.DownloadErr
	}
	if len(f.DownloadedData) > 0 {
		_, err := io.Copy(w, bytes.NewReader(f.DownloadedData))
		return err
	}
	return nil
}

func (f *FakeVMSandbox) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Closed = true
	return f.CloseErr
}

// FakeVMProvider creates FakeVMSandbox instances for testing.
type FakeVMProvider struct {
	mu               sync.Mutex
	CreateErr        error
	NextSandbox      *FakeVMSandbox
	CreatedSandboxes []*FakeVMSandbox
}

func NewFakeVMProvider() *FakeVMProvider {
	return &FakeVMProvider{}
}

func (p *FakeVMProvider) CreateSandbox(ctx context.Context, cfg *AgentConfig) (VMSandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.CreateErr != nil {
		return nil, p.CreateErr
	}

	sb := p.NextSandbox
	if sb == nil {
		sb = NewFakeVMSandbox(cfg.ID + "-vm")
	}
	p.CreatedSandboxes = append(p.CreatedSandboxes, sb)
	return sb, nil
}
