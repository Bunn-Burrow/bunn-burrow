package vmm

import (
	"context"
	"fmt"

	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/sirupsen/logrus"
)

// API is the subset of the Firecracker API the lifecycle needs. It is an
// interface so the lifecycle can be tested without a running VMM.
type API interface {
	PutMachineConfiguration(ctx context.Context, cfg *models.MachineConfiguration) error
	PutGuestBootSource(ctx context.Context, src *models.BootSource) error
	PutGuestDriveByID(ctx context.Context, driveID string, d *models.Drive) error
	PutGuestVsock(ctx context.Context, vsock *models.Vsock) error
	CreateSyncAction(ctx context.Context, action *models.InstanceActionInfo) error
	PatchVM(ctx context.Context, vm *models.VM) error
	CreateSnapshot(ctx context.Context, p *models.SnapshotCreateParams) error
	LoadSnapshot(ctx context.Context, p *models.SnapshotLoadParams) error
	GetInstanceInfo(ctx context.Context) (*models.InstanceInfo, error)
	GetMachineConfiguration(ctx context.Context) (*models.MachineConfiguration, error)
}

// apiClient wraps the official Firecracker Go SDK client.
type apiClient struct {
	client *firecracker.Client
}

// NewAPIClient creates an API client connected to the Firecracker Unix socket.
func NewAPIClient(socketPath string, logger *logrus.Entry) (API, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("api socket path is required")
	}
	return &apiClient{client: firecracker.NewClient(socketPath, logger, false)}, nil
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
