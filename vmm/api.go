package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

// API is the subset of the Firecracker API the lifecycle needs. It is an
// interface so the lifecycle can be tested without a running VMM.
type API interface {
	PutMachineConfiguration(ctx context.Context, cfg *models.MachineConfiguration) error
	PutGuestBootSource(ctx context.Context, src *models.BootSource) error
	PutGuestDriveByID(ctx context.Context, driveID string, d *models.Drive) error
	PutGuestNetworkInterfaceByID(ctx context.Context, ifaceID string, ni *models.NetworkInterface) error
	PutGuestVsock(ctx context.Context, vsock *models.Vsock) error
	CreateSyncAction(ctx context.Context, action *models.InstanceActionInfo) error
	PatchVM(ctx context.Context, vm *models.VM) error
	CreateSnapshot(ctx context.Context, p *models.SnapshotCreateParams) error
	LoadSnapshot(ctx context.Context, p *models.SnapshotLoadParams) error
	LoadSnapshotWithOverride(ctx context.Context, snapshotPath, memFilePath string, resumeVM bool, udsPath string) error
	GetInstanceInfo(ctx context.Context) (*models.InstanceInfo, error)
	GetMachineConfiguration(ctx context.Context) (*models.MachineConfiguration, error)
}

// apiClient wraps the official Firecracker Go SDK client.
type apiClient struct {
	client     *firecracker.Client
	socketPath string
}

// NewAPIClient creates an API client connected to the Firecracker Unix socket.
func NewAPIClient(socketPath string, logger *logrus.Entry) (API, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("api socket path is required")
	}
	return &apiClient{
		client:     firecracker.NewClient(socketPath, logger, false),
		socketPath: socketPath,
	}, nil
}

func (a *apiClient) PutMachineConfiguration(ctx context.Context, cfg *models.MachineConfiguration) error {
	_, err := a.client.PutMachineConfiguration(ctx, cfg)
	return err
}

func (a *apiClient) PutGuestBootSource(ctx context.Context, src *models.BootSource) error {
	_, err := a.client.PutGuestBootSource(ctx, src)
	return err
}

func (a *apiClient) PutGuestDriveByID(ctx context.Context, driveID string, d *models.Drive) error {
	_, err := a.client.PutGuestDriveByID(ctx, driveID, d)
	return err
}

func (a *apiClient) PutGuestNetworkInterfaceByID(ctx context.Context, ifaceID string, ni *models.NetworkInterface) error {
	_, err := a.client.PutGuestNetworkInterfaceByID(ctx, ifaceID, ni)
	return err
}

func (a *apiClient) PutGuestVsock(ctx context.Context, vsock *models.Vsock) error {
	_, err := a.client.PutGuestVsock(ctx, vsock)
	return err
}

func (a *apiClient) CreateSyncAction(ctx context.Context, action *models.InstanceActionInfo) error {
	_, err := a.client.CreateSyncAction(ctx, action)
	return err
}

func (a *apiClient) PatchVM(ctx context.Context, vm *models.VM) error {
	_, err := a.client.PatchVM(ctx, vm)
	return err
}

func (a *apiClient) CreateSnapshot(ctx context.Context, p *models.SnapshotCreateParams) error {
	_, err := a.client.CreateSnapshot(ctx, p)
	return err
}

func (a *apiClient) LoadSnapshot(ctx context.Context, p *models.SnapshotLoadParams) error {
	_, err := a.client.LoadSnapshot(ctx, p)
	return err
}

// loadSnapshotOverrideBody is the request body for PUT /snapshot/load with a
// vsock override. It mirrors the Firecracker v1.17.0 SnapshotLoadParams schema
// plus the nested vsock_override.uds_path field that the pinned Go SDK does not
// expose.
type loadSnapshotOverrideBody struct {
	SnapshotPath  string `json:"snapshot_path"`
	MemFilePath   string `json:"mem_file_path"`
	ResumeVM      bool   `json:"resume_vm"`
	VsockOverride struct {
		UdsPath string `json:"uds_path"`
	} `json:"vsock_override"`
}

// unixDialer returns an http.Transport that dials the given Unix socket path.
func unixDialer(socketPath string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
}

// LoadSnapshotWithOverride sends PUT /snapshot/load with a nested
// vsock_override.uds_path so the restored VM binds to a child-specific vsock
// Unix socket instead of inheriting the source VM's path. This is necessary
// because Firecracker v1.17.0 captures the vsock UDS path in the snapshot and
// the pinned Go SDK does not expose vsock_override in SnapshotLoadParams.
//
// Firecracker v1.17.0 accepts this field at the REST API level. LoadSnapshot
// is pre-boot only, so this must be called before any device configuration.
func (a *apiClient) LoadSnapshotWithOverride(ctx context.Context, snapshotPath, memFilePath string, resumeVM bool, udsPath string) error {
	body := loadSnapshotOverrideBody{
		SnapshotPath: snapshotPath,
		MemFilePath:  memFilePath,
		ResumeVM:     resumeVM,
	}
	body.VsockOverride.UdsPath = udsPath

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal load snapshot request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/snapshot/load", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create load snapshot request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{
		Transport: unixDialer(a.socketPath),
		Timeout:   30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("load snapshot failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
}

func (a *apiClient) GetInstanceInfo(ctx context.Context) (*models.InstanceInfo, error) {
	resp, err := a.client.GetInstanceInfo(ctx)
	if err != nil {
		return nil, err
	}
	return resp.Payload, nil
}

func (a *apiClient) GetMachineConfiguration(ctx context.Context) (*models.MachineConfiguration, error) {
	resp, err := a.client.GetMachineConfiguration()
	if err != nil {
		return nil, err
	}
	return resp.Payload, nil
}
