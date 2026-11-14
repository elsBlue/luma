package adb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type Device struct {
	Serial  string
	State   string
	Model   string
	Product string
}

func (d Device) Label() string {
	name := d.Model
	if name == "" {
		name = d.Product
	}
	if name == "" {
		name = d.Serial
	}
	name = strings.ReplaceAll(name, "_", " ")
	switch d.State {
	case "device":
		return name
	case "unauthorized":
		return name + " · tap Allow on the phone"
	case "offline":
		return name + " · offline"
	default:
		return name + " · " + d.State
	}
}

type Client struct {
	Path string
}

func (c *Client) bin() string {
	if c.Path != "" {
		return c.Path
	}
	return "adb"
}

func (c *Client) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	hide(cmd)
	return cmd
}

func (c *Client) Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := c.command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s", msg)
	}
	return out, nil
}

func (c *Client) StartServer(ctx context.Context) error {
	_, err := c.Output(ctx, "start-server")
	return err
}

func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	out, err := c.Output(ctx, "devices", "-l")
	if err != nil {
		return nil, err
	}
	return ParseDevices(string(out)), nil
}

func ParseDevices(text string) []Device {
	var list []Device
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := Device{Serial: fields[0], State: fields[1]}
		for _, f := range fields[2:] {
			if strings.HasPrefix(f, "model:") {
				d.Model = strings.TrimPrefix(f, "model:")
			}
			if strings.HasPrefix(f, "product:") {
				d.Product = strings.TrimPrefix(f, "product:")
			}
		}
		list = append(list, d)
	}
	return list
}

func (c *Client) Push(ctx context.Context, serial, local, remote string) error {
	_, err := c.Output(ctx, "-s", serial, "push", local, remote)
	return err
}

func (c *Client) Install(ctx context.Context, serial, apk string) error {
	_, err := c.Output(ctx, "-s", serial, "install", "-r", apk)
	return err
}

func (c *Client) Forward(ctx context.Context, serial, local, remote string) error {
	_, err := c.Output(ctx, "-s", serial, "forward", local, remote)
	return err
}

func (c *Client) ForwardRemove(ctx context.Context, serial, local string) {
	_, _ = c.Output(ctx, "-s", serial, "forward", "--remove", local)
}

func (c *Client) Reverse(ctx context.Context, serial, remote, local string) error {
	_, err := c.Output(ctx, "-s", serial, "reverse", remote, local)
	return err
}

func (c *Client) ReverseRemove(ctx context.Context, serial, remote string) {
	_, _ = c.Output(ctx, "-s", serial, "reverse", "--remove", remote)
}

func (c *Client) Shell(ctx context.Context, serial, cmdline string) (string, error) {
	out, err := c.Output(ctx, "-s", serial, "shell", cmdline)
	return string(out), err
}

func (c *Client) StartShell(ctx context.Context, serial, cmdline string) (*exec.Cmd, io.ReadCloser, error) {
	cmd := c.command(ctx, "-s", serial, "shell", cmdline)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, stdout, nil
}

func (c *Client) ExecOut(ctx context.Context, serial string, args ...string) *exec.Cmd {
	all := append([]string{"-s", serial, "exec-out"}, args...)
	return c.command(ctx, all...)
}

func (c *Client) TCPIP(ctx context.Context, serial string) error {
	_, err := c.Output(ctx, "-s", serial, "tcpip", "5555")
	return err
}

func (c *Client) Connect(ctx context.Context, addr string) error {
	out, err := c.Output(ctx, "connect", addr)
	if err != nil {
		return err
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "failed") || strings.Contains(low, "cannot") {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (c *Client) WLANIP(ctx context.Context, serial string) (string, error) {
	out, err := c.Shell(ctx, serial, "ip -f inet addr show wlan0")
	if err != nil || !strings.Contains(out, "inet ") {
		out, err = c.Shell(ctx, serial, "ip -f inet addr")
		if err != nil {
			return "", err
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "inet ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip := strings.Split(fields[1], "/")[0]
		if ip != "" && ip != "127.0.0.1" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("phone has no Wi-Fi address")
}
