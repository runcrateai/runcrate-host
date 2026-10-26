package vm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// SerialLog reads the VM's serial console ring buffer (last 1 MB) over QMP.
func SerialLog(sock string) (string, error) {
	c, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("qmp: %w", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	dec := json.NewDecoder(r)
	var greet map[string]any
	if err := dec.Decode(&greet); err != nil {
		return "", fmt.Errorf("qmp greeting: %w", err)
	}
	call := func(cmd string, args map[string]any) (json.RawMessage, error) {
		req := map[string]any{"execute": cmd}
		if args != nil {
			req["arguments"] = args
		}
		if err := json.NewEncoder(c).Encode(req); err != nil {
			return nil, err
		}
		for {
			var resp struct {
				Return json.RawMessage `json:"return"`
				Error  *struct {
					Desc string `json:"desc"`
				} `json:"error"`
				Event string `json:"event"`
			}
			if err := dec.Decode(&resp); err != nil {
				return nil, err
			}
			if resp.Event != "" {
				continue
			}
			if resp.Error != nil {
				return nil, fmt.Errorf("qmp %s: %s", cmd, resp.Error.Desc)
			}
			return resp.Return, nil
		}
	}
	if _, err := call("qmp_capabilities", nil); err != nil {
		return "", err
	}
	raw, err := call("ringbuf-read", map[string]any{"device": "serial0", "size": 1 << 20, "format": "utf8"})
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}
