package lxc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client wraps the LXD REST API.
type Client struct {
	http   *http.Client
	baseURL string
}

// NewClient creates a new LXD API client.
// socketPath is the path to the LXD Unix socket, e.g. /var/snap/lxd/common/lxd/unix.socket
func NewClient(socketPath string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				Dial: func(_, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
		baseURL: "http://unix/1.0",
	}
}

// ==================== Container ====================

type Container struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	State  struct {
		Memory int64 `json:"memory"`
		CPU    int64 `json:"cpu"`
	} `json:"state"`
}

type CreateInstanceReq struct {
	Name      string            `json:"name"`
	Source    InstanceSource    `json:"source"`
	Config    map[string]string `json:"config"`
	Devices   map[string]Device `json:"devices,omitempty"`
	Type      string            `json:"type,omitempty"`
}

type InstanceSource struct {
	Type  string `json:"type"`
	Alias string `json:"alias"`
}

type Device struct {
	Type    string `json:"type"`
	NicType string `json:"nictype,omitempty"`
	Parent  string `json:"parent,omitempty"`
}

type ExecReq struct {
	Command   []string `json:"command"`
	WaitForWS bool     `json:"wait-for-websocket"`
	Interactive bool   `json:"interactive"`
}

type OperationResp struct {
	Type       string `json:"type"`
	Status     string `json:"status"`
	StatusCode int    `json:"status_code"`
	Metadata   struct {
		ID string `json:"id"`
	} `json:"metadata"`
}

type StateReq struct {
	Action  string `json:"action"`
	Timeout int    `json:"timeout"`
}

// ==================== API Methods ====================

func (c *Client) CreateContainer(name, image, network string, cpu int, memMB int) error {
	req := CreateInstanceReq{
		Name: name,
		Source: InstanceSource{
			Type:  "image",
			Alias: image,
		},
		Config: map[string]string{
			"limits.cpu":    fmt.Sprintf("%d", cpu),
			"limits.memory": fmt.Sprintf("%dMB", memMB),
		},
	}
	return c.doJSON("POST", "/instances", req, nil)
}

func (c *Client) StartContainer(name string) error {
	return c.doJSON("PUT", fmt.Sprintf("/instances/%s/state", name),
		StateReq{Action: "start", Timeout: 30}, nil)
}

func (c *Client) StopContainer(name string) error {
	return c.doJSON("PUT", fmt.Sprintf("/instances/%s/state", name),
		StateReq{Action: "stop", Timeout: 30}, nil)
}

func (c *Client) DeleteContainer(name string) error {
	return c.do("DELETE", fmt.Sprintf("/instances/%s", name), nil)
}

func (c *Client) GetContainer(name string) (*Container, error) {
	var resp struct {
		Metadata Container `json:"metadata"`
	}
	if err := c.doJSON("GET", fmt.Sprintf("/instances/%s", name), nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Metadata, nil
}

func (c *Client) ListContainers(prefix string) ([]Container, error) {
	var resp struct {
		Metadata []Container `json:"metadata"`
	}
	url := "/instances"
	if prefix != "" {
		url += "?recursion=1"
	}
	if err := c.doJSON("GET", url, nil, &resp); err != nil {
		return nil, err
	}
	if prefix == "" {
		return resp.Metadata, nil
	}
	var filtered []Container
	for _, c := range resp.Metadata {
		if len(c.Name) >= len(prefix) && c.Name[:len(prefix)] == prefix {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

func (c *Client) ResizeContainer(name string, cpu int, memMB int) error {
	req := map[string]string{
		"config": fmt.Sprintf("{\"limits.cpu\":\"%d\",\"limits.memory\":\"%dMB\"}", cpu, memMB),
	}
	return c.doJSON("PATCH", fmt.Sprintf("/instances/%s", name), req, nil)
}

func (c *Client) Exec(name string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	req := ExecReq{
		Command:   cmd,
		WaitForWS: true,
	}
	var resp OperationResp
	if err := c.doJSON("POST", fmt.Sprintf("/instances/%s/exec", name), req, &resp); err != nil {
		return err
	}
	// Simplified: real implementation would use WebSocket
	return nil
}

// ==================== HTTP Helpers ====================

func (c *Client) doJSON(method, path string, body, result interface{}) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("LXD API error %d: %s", resp.StatusCode, string(b))
	}
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

func (c *Client) do(method, path string, result interface{}) error {
	return c.doJSON(method, path, nil, result)
}
