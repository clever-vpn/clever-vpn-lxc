package lxc

import (
	"fmt"
	"strings"
	"time"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
)

type Client struct {
	server lxd.InstanceServer
}

type Container struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	State  struct {
		Memory int64 `json:"memory"`
		CPU    int64 `json:"cpu"`
	} `json:"state"`
}

func NewClient(lxdURL string, clientCert string, clientKey string) (*Client, error) {
	args := &lxd.ConnectionArgs{
		TLSClientCert:      clientCert,
		TLSClientKey:       clientKey,
		InsecureSkipVerify: true, // we trust nodes by SSH provisioning, not by server cert
	}
	server, err := lxd.ConnectLXD(lxdURL, args)
	if err != nil {
		return nil, fmt.Errorf("connect lxd %s: %w", lxdURL, err)
	}
	return &Client{server: server}, nil
}

func (c *Client) CreateContainer(name, image, network string, cpu int, memMB int, diskGB int, config map[string]string) error {
	devices := map[string]map[string]string{
		"eth0": {
			"type":    "nic",
			"network": network,
		},
	}
	if diskGB > 0 {
		devices["root"] = map[string]string{
			"type": "disk",
			"path": "/",
			"pool": "default",
			"size": fmt.Sprintf("%dGB", diskGB),
		}
	}

	post := api.InstancesPost{
		Name: name,
		Type: "container",
		InstancePut: api.InstancePut{
			Config:  config,
			Devices: devices,
		},
		Source: api.InstanceSource{
			Type:  "image",
			Alias: image,
		},
	}
	if post.Config == nil {
		post.Config = map[string]string{}
	}
	post.Config["limits.cpu"] = fmt.Sprintf("%d", cpu)
	post.Config["limits.memory"] = fmt.Sprintf("%dMB", memMB)
	// Enable nesting so eBPF programs can load in the container.
	post.Config["security.nesting"] = "true"
	// Allow eBPF programs to lock required memory.
	post.Config["limits.kernel.memlock"] = "unlimited"
	// BTF loading (BPF_BTF_GET_FD_BY_ID) requires CAP_SYS_ADMIN.
	post.Config["security.privileged"] = "true"

	op, err := c.server.CreateInstance(post)
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *Client) StartContainer(name string) error {
	op, err := c.server.UpdateInstanceState(name, api.InstanceStatePut{
		Action:  "start",
		Timeout: 30,
	}, "")
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *Client) StopContainer(name string) error {
	op, err := c.server.UpdateInstanceState(name, api.InstanceStatePut{
		Action:  "stop",
		Timeout: 30,
		Force:   true,
	}, "")
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *Client) DeleteContainer(name string) error {
	op, err := c.server.DeleteInstance(name, true)
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *Client) GetContainer(name string) (*Container, error) {
	inst, _, err := c.server.GetInstance(name)
	if err != nil {
		return nil, err
	}
	state, _, err := c.server.GetInstanceState(name)
	if err != nil {
		return nil, err
	}
	return toContainer(inst, state), nil
}

func (c *Client) ListContainers(prefix string) ([]Container, error) {
	insts, err := c.server.GetInstances(lxd.GetInstancesArgs{
		InstanceType: api.InstanceTypeContainer,
	})
	if err != nil {
		return nil, err
	}

	containers := make([]Container, 0, len(insts))
	for _, inst := range insts {
		if prefix != "" && !strings.HasPrefix(inst.Name, prefix) {
			continue
		}
		container := Container{
			Name:   inst.Name,
			Status: inst.Status,
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func (c *Client) ResizeContainer(name string, cpu int, memMB int, diskGB int) error {
	inst, etag, err := c.server.GetInstance(name)
	if err != nil {
		return err
	}

	put := inst.Writable()
	if put.Config == nil {
		put.Config = map[string]string{}
	}
	put.Config["limits.cpu"] = fmt.Sprintf("%d", cpu)
	put.Config["limits.memory"] = fmt.Sprintf("%dMB", memMB)

	if diskGB > 0 {
		if put.Devices == nil {
			put.Devices = map[string]map[string]string{}
		}
		root, ok := put.Devices["root"]
		if !ok {
			root = map[string]string{"type": "disk", "path": "/"}
		}
		root["size"] = fmt.Sprintf("%dGB", diskGB)
		put.Devices["root"] = root
	}

	op, err := c.server.UpdateInstance(name, put, etag)
	if err != nil {
		return err
	}
	return op.Wait()
}

func (c *Client) InstanceIPv4(name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, _, err := c.server.GetInstanceState(name)
		if err == nil {
			for _, network := range state.Network {
				for _, addr := range network.Addresses {
					if addr.Family == "inet" && addr.Scope == "global" && addr.Address != "" {
						return addr.Address, nil
					}
				}
			}
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("timed out waiting for IPv4 for %s", name)
}

func toContainer(inst *api.Instance, state *api.InstanceState) *Container {
	container := &Container{
		Name:   inst.Name,
		Status: inst.Status,
	}
	if state != nil {
		container.State.Memory = state.Memory.Usage
		container.State.CPU = state.CPU.Usage
	}
	return container
}
