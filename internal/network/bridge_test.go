package network

import (
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

type fakeBridgeLinkOps struct {
	links        map[string]netlink.Link
	attached     bool
	isolated     bool
	isolationErr error
	detachErr    error
}

func (o *fakeBridgeLinkOps) LinkByName(name string) (netlink.Link, error) {
	link, ok := o.links[name]
	if !ok {
		return nil, netlink.LinkNotFoundError{}
	}
	return link, nil
}

func (o *fakeBridgeLinkOps) LinkSetMaster(netlink.Link, netlink.Link) error {
	o.attached = true
	return nil
}

func (o *fakeBridgeLinkOps) LinkSetNoMaster(netlink.Link) error {
	o.attached = false
	return o.detachErr
}

func (o *fakeBridgeLinkOps) LinkSetIsolated(_ netlink.Link, mode bool) error {
	o.isolated = mode
	return o.isolationErr
}

func TestBridgeAddPortEnablesIsolation(t *testing.T) {
	ops := &fakeBridgeLinkOps{links: map[string]netlink.Link{
		"tap-test": &netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: "tap-test"}},
		"br-test":  &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br-test"}},
	}}
	bridge := NewBridgeManager("br-test", logrus.NewEntry(logrus.New()))
	bridge.ops = ops
	bridge.useHelper = false

	require.NoError(t, bridge.AddPort("tap-test"))
	require.True(t, ops.attached)
	require.True(t, ops.isolated)
}

func TestBridgeAddPortReturnsIsolationFailureAndDetaches(t *testing.T) {
	isolationErr := errors.New("isolation unsupported")
	ops := &fakeBridgeLinkOps{
		links: map[string]netlink.Link{
			"tap-test": &netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: "tap-test"}},
			"br-test":  &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br-test"}},
		},
		isolationErr: isolationErr,
	}
	bridge := NewBridgeManager("br-test", logrus.NewEntry(logrus.New()))
	bridge.ops = ops
	bridge.useHelper = false

	err := bridge.AddPort("tap-test")
	require.ErrorIs(t, err, isolationErr)
	require.True(t, ops.isolated)
	require.False(t, ops.attached)
}

func TestBridgeAddPortReportsDetachFailure(t *testing.T) {
	isolationErr := errors.New("isolation unsupported")
	detachErr := errors.New("detach failed")
	ops := &fakeBridgeLinkOps{
		links: map[string]netlink.Link{
			"tap-test": &netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: "tap-test"}},
			"br-test":  &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br-test"}},
		},
		isolationErr: isolationErr,
		detachErr:    detachErr,
	}
	bridge := NewBridgeManager("br-test", logrus.NewEntry(logrus.New()))
	bridge.ops = ops
	bridge.useHelper = false

	err := bridge.AddPort("tap-test")
	require.ErrorIs(t, err, isolationErr)
	require.ErrorContains(t, err, detachErr.Error())
}
