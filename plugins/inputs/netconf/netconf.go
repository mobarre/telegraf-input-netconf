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
	"sync"

	"github.com/Juniper/go-netconf/netconf"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
	"golang.org/x/crypto/ssh"
)

// Device represents a single NETCONF device.
type Device struct {
	Address  string `toml:"address"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// Netconf is the main plugin struct.
type Netconf struct {
	Devices []Device `toml:"devices"`

	// Map to store active connections (address -> session)
	connections map[string]*netconf.Session
	mu          sync.Mutex
}

// SampleConfig returns the default configuration for the plugin.
func (n *Netconf) SampleConfig() string {
	return `
		## List of NETCONF devices to poll
		[[inputs.netconf.devices]]
		  address = "192.168.1.1:830"
		  username = "admin"
		  password = "password"
		# [[inputs.netconf.devices]]
		#   address = "192.168.1.2:830"
		#   username = "admin"
		#   password = "password"
	`
}

// Description returns a description of the plugin.
func (n *Netconf) Description() string {
	return "Collects interface input/output bytes from NETCONF-enabled devices (Cisco/Juniper)"
}

// Gather collects metrics from all devices.
func (n *Netconf) Gather(acc telegraf.Accumulator) error {
	for _, device := range n.Devices {
		session, err := n.connect(device)
		if err != nil {
			acc.AddError(fmt.Errorf("failed to connect to %s: %v", device.Address, err))
			continue
		}

		rpc := `
			<filter xmlns="urn:ietf:params:xml:ns:netconf:base:1.0" type="subtree">
				<interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces">
					<interface>
						<statistics/>
					</interface>
				</interfaces>
			</filter>
		`
		reply, err := session.Exec(netconf.RawMethod(rpc))
		if err != nil {
			return fmt.Errorf("RPC failed: %v", err)
		}
		// Get the raw XML reply as []byte
		replyBytes := []byte(reply.Data)

		// Parse the XML reply
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
		//		if err := xml.Unmarshal([]byte(reply), &result); err != nil {
		//			acc.AddError(fmt.Errorf("XML parse failed for %s: %v", device.Address, err))
		//			continue
		//		}
		if err := xml.Unmarshal(replyBytes, &result); err != nil {
			acc.AddError(fmt.Errorf("XML parse failed for %s: %v", device.Address, err))
			continue
		}

		// Add metrics to the accumulator
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

// connect ensures a connection exists for the given device.
func (n *Netconf) connect(device Device) (*netconf.Session, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Reuse existing connection if available
	if session, ok := n.connections[device.Address]; ok {
		return session, nil
	}

	// Configure SSH client
	sshConfig := &ssh.ClientConfig{
		User: device.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(device.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // Use a secure callback in production!
	}

	// Open a new connection
	session, err := netconf.DialSSH(device.Address, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %v", device.Address, err)
	}

	// Store the connection
	n.connections[device.Address] = session
	return session, nil
}

// disconnectAll closes all active connections.
func (n *Netconf) disconnectAll() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	for addr, session := range n.connections {
		session.Close()
		delete(n.connections, addr)
	}
	return nil
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
