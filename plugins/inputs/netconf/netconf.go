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
// from NETCONF-enabled devices (Cisco, Juniper, etc.). It runs as a single
// daemon that keeps one warm SSH/NETCONF session per device and fans out
// across devices concurrently. See DESIGN.md for the rationale.

package netconf

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/Juniper/go-netconf/netconf"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/influxdata/telegraf/plugins/parsers/xpath"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	// defaultTimeout bounds both connecting to and polling a device.
	defaultTimeout = 10 * time.Second
	// defaultSessionMaxAge is how long a session is reused before it is
	// proactively recycled.
	defaultSessionMaxAge = time.Hour
	// defaultMaxConcurrentDevices caps how many devices are dialed/polled at once.
	defaultMaxConcurrentDevices = 10
)

// ncSession is the minimal NETCONF session surface the plugin needs.
// *netconf.Session satisfies it; tests substitute fakes.
type ncSession interface {
	Exec(methods ...netconf.RPCMethod) (*netconf.RPCReply, error)
	Close() error
}

// dialFunc opens a session to a device. It is a field on Netconf so tests can
// inject a fake dialer instead of touching the network.
type dialFunc func(device Device, cfg *ssh.ClientConfig, timeout time.Duration) (ncSession, error)

// dialNetconf is the production dialer.
func dialNetconf(device Device, cfg *ssh.ClientConfig, timeout time.Duration) (ncSession, error) {
	sess, err := netconf.DialSSHTimeout(device.Address, cfg, timeout)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// Device represents a single NETCONF device.
type Device struct {
	Address  string        `toml:"address"`
	Username string        `toml:"username"`
	Password config.Secret `toml:"password"`
}

// Sensor pairs a NETCONF RPC with an XPath mapping that turns the reply into
// metrics. The embedded xpath.Config exposes the standard Telegraf xpath
// options (metric_name, metric_selection, tags, fields, fields_int, …)
// directly as keys of the [[inputs.netconf.sensor]] table.
type Sensor struct {
	// RPC is the NETCONF operation payload sent to each device. go-netconf
	// wraps it in <rpc>…</rpc>, so provide the inner operation (e.g. a <get>).
	RPC string `toml:"rpc"`

	xpath.Config

	parser *xpath.Parser
}

// deviceConn holds the persistent session for one device. mu serializes RPCs
// on the session (a NETCONF session is a single SSH channel and is not safe for
// concurrent use).
type deviceConn struct {
	mu       sync.Mutex
	session  ncSession
	dialedAt time.Time
}

// Netconf is the main plugin struct.
type Netconf struct {
	Devices []Device `toml:"devices"`
	Sensors []Sensor `toml:"sensor"`

	// DevicesDirectory, when set, is scanned for *.toml files, each containing
	// one or more [[devices]] entries that are merged into Devices at Init.
	DevicesDirectory string `toml:"devices_directory"`

	// Timeout bounds both connecting to and polling each device.
	Timeout config.Duration `toml:"timeout"`
	// SessionMaxAge is how long a session is reused before being recycled.
	SessionMaxAge config.Duration `toml:"session_max_age"`
	// MaxConcurrentDevices caps simultaneous dials/polls (0 = unlimited).
	MaxConcurrentDevices int `toml:"max_concurrent_devices"`

	// KnownHostsFile is an OpenSSH known_hosts file used to verify device host
	// keys. Required unless InsecureSkipVerify is set — there is deliberately no
	// default, because the telegraf user often has no home directory.
	KnownHostsFile string `toml:"known_hosts_file"`
	// InsecureSkipVerify disables host key verification entirely. Lab use only.
	InsecureSkipVerify bool `toml:"insecure_skip_verify"`

	Log telegraf.Logger `toml:"-"`

	hostKeyCallback ssh.HostKeyCallback
	dial            dialFunc

	// conns stores the persistent session per device address.
	conns map[string]*deviceConn
	mu    sync.Mutex
}

// SampleConfig returns the default configuration for the plugin.
func (n *Netconf) SampleConfig() string {
	return `
  ## Timeout for connecting to and polling each device.
  # timeout = "10s"

  ## How long a session is reused before it is proactively recycled.
  # session_max_age = "1h"

  ## Maximum number of devices dialed/polled concurrently (0 = unlimited).
  ## Matters most at start-up and recycle, where it bounds the dial burst.
  # max_concurrent_devices = 10

  ## Path to an OpenSSH known_hosts file used to verify device host keys.
  ## Required unless insecure_skip_verify is set (no default: the telegraf user
  ## usually has no home directory).
  known_hosts_file = "/etc/telegraf/known_hosts"

  ## Disable host key verification entirely. INSECURE - lab use only.
  # insecure_skip_verify = false

  ## Devices may be listed inline and/or loaded from a directory of *.toml
  ## files (each with one or more [[devices]] entries). Both are merged.
  # devices_directory = "/etc/telegraf/netconf.d"

  [[inputs.netconf.devices]]
    address = "192.168.1.1:830"
    username = "admin"
    ## Password supports Telegraf secret-store references, e.g. "@{store:key}".
    password = "password"

  ## A sensor is a NETCONF RPC plus an XPath mapping describing how to turn the
  ## reply into metrics. Add as many as you need (interfaces, BGP, system, …).
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

// Init validates the configuration, merges devices, builds a parser per sensor,
// and builds the SSH host key callback once.
func (n *Netconf) Init() error {
	if n.conns == nil {
		n.conns = make(map[string]*deviceConn)
	}
	if n.dial == nil {
		n.dial = dialNetconf
	}
	if n.Timeout <= 0 {
		n.Timeout = config.Duration(defaultTimeout)
	}
	if n.SessionMaxAge <= 0 {
		n.SessionMaxAge = config.Duration(defaultSessionMaxAge)
	}
	if n.MaxConcurrentDevices < 0 {
		n.MaxConcurrentDevices = 0
	}

	if err := n.loadDeviceDirectory(); err != nil {
		return err
	}
	if len(n.Devices) == 0 {
		return errors.New("no devices configured (set [[inputs.netconf.devices]] and/or devices_directory)")
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

	return n.buildHostKeyCallback()
}

// loadDeviceDirectory merges devices from *.toml files in DevicesDirectory.
func (n *Netconf) loadDeviceDirectory() error {
	if n.DevicesDirectory == "" {
		return nil
	}
	entries, err := os.ReadDir(n.DevicesDirectory)
	if err != nil {
		return fmt.Errorf("reading devices_directory %q: %w", n.DevicesDirectory, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		path := filepath.Join(n.DevicesDirectory, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading device file %q: %w", path, err)
		}
		var df struct {
			Devices []Device `toml:"devices"`
		}
		if err := toml.Unmarshal(data, &df); err != nil {
			return fmt.Errorf("parsing device file %q: %w", path, err)
		}
		n.Devices = append(n.Devices, df.Devices...)
	}
	return nil
}

// buildHostKeyCallback constructs the SSH host key verification callback.
func (n *Netconf) buildHostKeyCallback() error {
	if n.InsecureSkipVerify {
		if n.Log != nil {
			n.Log.Warn("insecure_skip_verify is enabled; NETCONF host keys are NOT verified")
		}
		n.hostKeyCallback = ssh.InsecureIgnoreHostKey()
		return nil
	}
	if n.KnownHostsFile == "" {
		return errors.New("known_hosts_file must be set (or enable insecure_skip_verify); there is no default because the telegraf user often has no home directory")
	}
	cb, err := knownhosts.New(n.KnownHostsFile)
	if err != nil {
		return fmt.Errorf("loading known_hosts file %q: %w", n.KnownHostsFile, err)
	}
	n.hostKeyCallback = cb
	return nil
}

// Gather polls every device concurrently (bounded), reusing warm sessions.
func (n *Netconf) Gather(acc telegraf.Accumulator) error {
	limit := n.MaxConcurrentDevices
	if limit <= 0 || limit > len(n.Devices) {
		limit = len(n.Devices)
	}
	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup
	for i := range n.Devices {
		device := n.Devices[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			n.gatherDevice(acc, device)
		}()
	}
	wg.Wait()
	return nil
}

// gatherDevice runs every sensor against one device on its single session.
func (n *Netconf) gatherDevice(acc telegraf.Accumulator, device Device) {
	dc := n.deviceConn(device.Address)
	dc.mu.Lock()
	defer dc.mu.Unlock()

	session, err := n.ensureSession(dc, device)
	if err != nil {
		acc.AddError(fmt.Errorf("connecting to %s: %w", device.Address, err))
		return
	}

	for i := range n.Sensors {
		sensor := &n.Sensors[i]

		reply, err := n.exec(session, sensor.RPC)
		if err != nil {
			// The session may be dead; drop it so the next cycle reconnects.
			n.closeConn(dc)
			acc.AddError(fmt.Errorf("RPC failed for %s: %w", device.Address, err))
			return
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

// deviceConn returns the per-device connection holder, creating it on first use.
func (n *Netconf) deviceConn(address string) *deviceConn {
	n.mu.Lock()
	defer n.mu.Unlock()
	dc, ok := n.conns[address]
	if !ok {
		dc = &deviceConn{}
		n.conns[address] = dc
	}
	return dc
}

// ensureSession returns a live session for the device, recycling an aged one
// and dialing a new one when needed. Must be called with dc.mu held.
func (n *Netconf) ensureSession(dc *deviceConn, device Device) (ncSession, error) {
	if dc.session != nil && time.Since(dc.dialedAt) > time.Duration(n.SessionMaxAge) {
		dc.session.Close()
		dc.session = nil
	}
	if dc.session != nil {
		return dc.session, nil
	}

	session, err := n.dialDevice(device)
	if err != nil {
		return nil, err
	}
	dc.session = session
	dc.dialedAt = time.Now()
	return session, nil
}

// dialDevice builds the SSH client config and opens a session.
func (n *Netconf) dialDevice(device Device) (ncSession, error) {
	password, err := device.Password.Get()
	if err != nil {
		return nil, fmt.Errorf("retrieving password: %w", err)
	}
	defer password.Destroy()

	cfg := &ssh.ClientConfig{
		User: device.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(password.TemporaryString()),
		},
		HostKeyCallback: n.hostKeyCallback,
		Timeout:         time.Duration(n.Timeout),
	}
	return n.dial(device, cfg, time.Duration(n.Timeout))
}

// closeConn closes and forgets the session held by dc. Must be called with
// dc.mu held.
func (n *Netconf) closeConn(dc *deviceConn) {
	if dc.session != nil {
		dc.session.Close()
		dc.session = nil
	}
}

// exec runs an RPC against the session, bounded by the configured timeout.
// go-netconf's Exec has no context support, so we race it against a timer. The
// result channel is buffered so the RPC goroutine never leaks on timeout; it
// unblocks once the (subsequently dropped) session is closed.
func (n *Netconf) exec(session ncSession, rpc string) (*netconf.RPCReply, error) {
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

// NewNetconf creates a new plugin instance.
func NewNetconf() *Netconf {
	return &Netconf{
		conns: make(map[string]*deviceConn),
		dial:  dialNetconf,
	}
}

func init() {
	inputs.Add("netconf", func() telegraf.Input {
		return NewNetconf()
	})
}

// Stop closes all active connections.
func (n *Netconf) Stop() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for addr, dc := range n.conns {
		dc.mu.Lock()
		if dc.session != nil {
			dc.session.Close()
			dc.session = nil
		}
		dc.mu.Unlock()
		delete(n.conns, addr)
	}
}
