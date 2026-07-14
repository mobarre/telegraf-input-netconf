// Copyright (C) 2025 Marc-Olivier Barre
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// Package netconf implements a Telegraf input plugin for collecting metrics
// from NETCONF-enabled devices (Cisco, Juniper, etc.).

package netconf

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Juniper/go-netconf/netconf"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/inputs"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// defaultTimeout is used for both connecting to and polling a device when no
// timeout is configured.
const defaultTimeout = 10 * time.Second

// getInterfaceStatistics is the NETCONF <get> request used to poll interface
// counters. Note: ietf-interfaces places operational counters under
// /interfaces-state on RFC 7223 devices and under /interfaces on RFC 8343
// (NMDA) devices; the response parsing below expects the latter. This has only
// been validated in a lab and may need adjusting per platform.
const getInterfaceStatistics = `<get>
  <filter type="subtree">
    <interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces">
      <interface>
        <statistics/>
      </interface>
    </interfaces>
  </filter>
</get>`

// Device represents a single NETCONF device.
type Device struct {
	Address  string        `toml:"address"`
	Username string        `toml:"username"`
	Password config.Secret `toml:"password"`
}

// Netconf is the main plugin struct.
type Netconf struct {
	Devices []Device `toml:"devices"`

	// Timeout bounds both connecting to and polling each device.
	Timeout config.Duration `toml:"timeout"`
	// KnownHostsFile is an OpenSSH known_hosts file used to verify device host
	// keys. Defaults to ~/.ssh/known_hosts when empty.
	KnownHostsFile string `toml:"known_hosts_file"`
	// InsecureSkipVerify disables host key verification entirely. Lab use only.
	InsecureSkipVerify bool `toml:"insecure_skip_verify"`

	Log telegraf.Logger `toml:"-"`

	// hostKeyCallback is built once in Init from the options above.
	hostKeyCallback ssh.HostKeyCallback

	// connections stores active sessions keyed by device address.
	connections map[string]*netconf.Session
	mu          sync.Mutex
}

// SampleConfig returns the default configuration for the plugin.
func (n *Netconf) SampleConfig() string {
	return `
  ## Timeout for connecting to and polling each device.
  # timeout = "10s"

  ## Path to an OpenSSH known_hosts file used to verify device host keys.
  ## Defaults to ~/.ssh/known_hosts.
  # known_hosts_file = "/etc/telegraf/known_hosts"

  ## Disable host key verification entirely. INSECURE - lab use only.
  # insecure_skip_verify = false

  ## One or more NETCONF devices to poll.
  [[inputs.netconf.devices]]
    address = "192.168.1.1:830"
    username = "admin"
    ## Password supports Telegraf secret-store references, e.g. "@{store:key}".
    password = "password"
`
}

// Init validates the configuration and builds the SSH host key callback once.
func (n *Netconf) Init() error {
	if n.connections == nil {
		n.connections = make(map[string]*netconf.Session)
	}
	if n.Timeout <= 0 {
		n.Timeout = config.Duration(defaultTimeout)
	}

	if n.InsecureSkipVerify {
		if n.Log != nil {
			n.Log.Warn("insecure_skip_verify is enabled; NETCONF host keys are NOT verified")
		}
		n.hostKeyCallback = ssh.InsecureIgnoreHostKey()
		return nil
	}

	path := n.KnownHostsFile
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory for known_hosts: %w", err)
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return fmt.Errorf("loading known_hosts file %q: %w (set insecure_skip_verify=true to bypass)", path, err)
	}
	n.hostKeyCallback = cb
	return nil
}

// Gather collects metrics from all devices.
func (n *Netconf) Gather(acc telegraf.Accumulator) error {
	for _, device := range n.Devices {
		session, err := n.connect(device)
		if err != nil {
			acc.AddError(fmt.Errorf("failed to connect to %s: %w", device.Address, err))
			continue
		}

		reply, err := n.exec(session, getInterfaceStatistics)
		if err != nil {
			// The session may be dead; drop it so the next cycle reconnects.
			n.dropConnection(device.Address)
			acc.AddError(fmt.Errorf("RPC failed for %s: %w", device.Address, err))
			continue
		}

		// Parse the XML reply.
		var result struct {
			Interfaces struct {
				Interface []struct {
					Name       string `xml:"name"`
					Statistics struct {
						InOctets  uint64 `xml:"in-octets"`
						OutOctets uint64 `xml:"out-octets"`
					} `xml:"statistics"`
				} `xml:"interface"`
			} `xml:"interfaces"`
		}
		if err := xml.Unmarshal([]byte(reply.Data), &result); err != nil {
			acc.AddError(fmt.Errorf("XML parse failed for %s: %w", device.Address, err))
			continue
		}

		// Add metrics to the accumulator.
		for _, iface := range result.Interfaces.Interface {
			tags := map[string]string{
				"interface": iface.Name,
				"device":    device.Address,
			}
			fields := map[string]interface{}{
				"input_bytes":  iface.Statistics.InOctets,
				"output_bytes": iface.Statistics.OutOctets,
			}
			acc.AddFields("netconf_interface", fields, tags)
		}
	}
	return nil
}

// exec runs an RPC against the session, bounded by the configured timeout.
// go-netconf's Exec has no context support, so we race it against a timer. The
// result channel is buffered so the RPC goroutine never leaks on timeout; it
// unblocks once the (subsequently dropped) session is closed.
func (n *Netconf) exec(session *netconf.Session, rpc string) (*netconf.RPCReply, error) {
	type result struct {
		reply *netconf.RPCReply
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		reply, err := session.Exec(netconf.RawMethod(rpc))
		ch <- result{reply, err}
	}()

	timeout := time.Duration(n.Timeout)
	select {
	case r := <-ch:
		return r.reply, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("RPC timed out after %s", timeout)
	}
}

// connect returns a live session for the device, reusing a cached one when
// available and dialing a new one otherwise.
func (n *Netconf) connect(device Device) (*netconf.Session, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Reuse existing connection if available.
	if session, ok := n.connections[device.Address]; ok {
		return session, nil
	}

	password, err := device.Password.Get()
	if err != nil {
		return nil, fmt.Errorf("retrieving password: %w", err)
	}
	defer password.Destroy()

	sshConfig := &ssh.ClientConfig{
		User: device.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(password.TemporaryString()),
		},
		HostKeyCallback: n.hostKeyCallback,
		Timeout:         time.Duration(n.Timeout),
	}

	session, err := netconf.DialSSHTimeout(device.Address, sshConfig, time.Duration(n.Timeout))
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", device.Address, err)
	}

	n.connections[device.Address] = session
	return session, nil
}

// dropConnection closes and forgets the session for the given address, if any.
func (n *Netconf) dropConnection(address string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if session, ok := n.connections[address]; ok {
		session.Close()
		delete(n.connections, address)
	}
}

// disconnectAll closes all active connections.
func (n *Netconf) disconnectAll() {
	n.mu.Lock()
	defer n.mu.Unlock()

	for addr, session := range n.connections {
		session.Close()
		delete(n.connections, addr)
	}
}

// NewNetconf creates a new plugin instance.
func NewNetconf() *Netconf {
	return &Netconf{
		connections: make(map[string]*netconf.Session),
	}
}

func init() {
	inputs.Add("netconf", func() telegraf.Input {
		return NewNetconf()
	})
}

// Stop closes all active connections.
func (n *Netconf) Stop() {
	n.disconnectAll()
}
