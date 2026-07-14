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
// from NETCONF-enabled devices (Cisco, Juniper, etc.). It is protocol-model
// agnostic: what to poll and how to turn replies into metrics is entirely
// driven by user-configured sensors.

package netconf

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Juniper/go-netconf/netconf"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/influxdata/telegraf/plugins/parsers/xpath"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// defaultTimeout is used for both connecting to and polling a device when no
// timeout is configured.
const defaultTimeout = 10 * time.Second

// Device represents a single NETCONF device.
type Device struct {
	Address  string        `toml:"address"`
	Username string        `toml:"username"`
	Password config.Secret `toml:"password"`
}

// Sensor pairs a NETCONF RPC with an XPath mapping that turns the reply into
// metrics. The embedded xpath.Config exposes the standard Telegraf xpath
// options (metric_name, metric_selection, tags, fields, fields_int, …)
// directly as keys of the [[inputs.netconf.sensor]] table, so the plugin is
// not tied to any particular data model.
type Sensor struct {
	// RPC is the NETCONF operation payload sent to each device. go-netconf
	// wraps it in <rpc>…</rpc>, so provide the inner operation (e.g. a <get>).
	RPC string `toml:"rpc"`

	xpath.Config

	parser *xpath.Parser
}

// Netconf is the main plugin struct.
type Netconf struct {
	Devices []Device `toml:"devices"`
	Sensors []Sensor `toml:"sensor"`

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

  ## One or more NETCONF devices to poll. Every sensor below is polled on
  ## every device.
  [[inputs.netconf.devices]]
    address = "192.168.1.1:830"
    username = "admin"
    ## Password supports Telegraf secret-store references, e.g. "@{store:key}".
    password = "password"

  ## A sensor is a NETCONF RPC plus an XPath mapping describing how to turn the
  ## reply into metrics. Add as many as you need (interfaces, BGP, system, …).
  ## The xpath options are documented at:
  ## https://github.com/influxdata/telegraf/tree/master/plugins/parsers/xpath
  ## Tip: match elements with local-name() to ignore NETCONF's XML namespaces.
  [[inputs.netconf.sensor]]
    rpc = '''
      <get>
        <filter type="subtree">
          <interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces"/>
        </filter>
      </get>
    '''
    metric_name = "'netconf_interface'"
    metric_selection = "//*[local-name()='interface']"
    [inputs.netconf.sensor.tags]
      interface = "*[local-name()='name']"
    [inputs.netconf.sensor.fields_int]
      input_bytes  = ".//*[local-name()='in-octets']"
      output_bytes = ".//*[local-name()='out-octets']"
`
}

// Init validates the configuration, builds a parser per sensor, and builds the
// SSH host key callback once.
func (n *Netconf) Init() error {
	if n.connections == nil {
		n.connections = make(map[string]*netconf.Session)
	}
	if n.Timeout <= 0 {
		n.Timeout = config.Duration(defaultTimeout)
	}
	if len(n.Sensors) == 0 {
		return errors.New("at least one sensor must be configured")
	}
	for i := range n.Sensors {
		s := &n.Sensors[i]
		if strings.TrimSpace(s.RPC) == "" {
			return fmt.Errorf("sensor %d: 'rpc' is required", i)
		}
		p := &xpath.Parser{
			Format:            "xml",
			Configs:           []xpath.Config{s.Config},
			DefaultMetricName: "netconf",
			Log:               n.Log,
		}
		if err := p.Init(); err != nil {
			return fmt.Errorf("sensor %d: %w", i, err)
		}
		s.parser = p
	}

	if err := n.buildHostKeyCallback(); err != nil {
		return err
	}
	return nil
}

// buildHostKeyCallback constructs the SSH host key verification callback from
// the configured options.
func (n *Netconf) buildHostKeyCallback() error {
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

// Gather collects metrics from every device for every configured sensor.
func (n *Netconf) Gather(acc telegraf.Accumulator) error {
	for _, device := range n.Devices {
		session, err := n.connect(device)
		if err != nil {
			acc.AddError(fmt.Errorf("failed to connect to %s: %w", device.Address, err))
			continue
		}

		for i := range n.Sensors {
			sensor := &n.Sensors[i]

			reply, err := n.exec(session, sensor.RPC)
			if err != nil {
				// The session may be dead; drop it so the next cycle reconnects
				// and skip the remaining sensors for this device this cycle.
				n.dropConnection(device.Address)
				acc.AddError(fmt.Errorf("RPC failed for %s: %w", device.Address, err))
				break
			}

			metrics, err := sensor.parser.Parse([]byte(reply.Data))
			if err != nil {
				acc.AddError(fmt.Errorf("parsing reply from %s: %w", device.Address, err))
				continue
			}
			for _, m := range metrics {
				m.AddTag("device", device.Address)
				acc.AddMetric(m)
			}
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
