// Copyright (C) 2025 Marc-Olivier Barre
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. See the LICENSE file for details.

package netconf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/parsers/xpath"
	itoml "github.com/influxdata/toml"
)

// noopLogger is a telegraf.Logger that discards everything. Using it instead of
// testutil.Logger keeps testcontainers/docker/otel out of the dependency graph.
type noopLogger struct{}

func (noopLogger) Level() telegraf.LogLevel         { return telegraf.Info }
func (noopLogger) AddAttribute(string, interface{}) {}
func (noopLogger) Errorf(string, ...interface{})    {}
func (noopLogger) Error(...interface{})             {}
func (noopLogger) Warnf(string, ...interface{})     {}
func (noopLogger) Warn(...interface{})              {}
func (noopLogger) Infof(string, ...interface{})     {}
func (noopLogger) Info(...interface{})              {}
func (noopLogger) Debugf(string, ...interface{})    {}
func (noopLogger) Debug(...interface{})             {}
func (noopLogger) Tracef(string, ...interface{})    {}
func (noopLogger) Trace(...interface{})             {}

// interfaceSensor returns the interface-statistics sensor used as the running
// example in the docs, built programmatically.
func interfaceSensor() Sensor {
	return Sensor{
		RPC: `<get><filter type="subtree"><interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces"/></filter></get>`,
		Config: xpath.Config{
			MetricQuery: "'netconf_interface'",
			Selection:   "//*[local-name()='interface']",
			Tags:        map[string]string{"interface": "*[local-name()='name']"},
			FieldsInt: map[string]string{
				"input_bytes":  ".//*[local-name()='in-octets']",
				"output_bytes": ".//*[local-name()='out-octets']",
			},
		},
	}
}

// newInitializedPlugin builds a plugin with the given sensors, host-key
// verification disabled, and a test logger, then runs Init.
func newInitializedPlugin(t *testing.T, sensors ...Sensor) *Netconf {
	t.Helper()
	n := NewNetconf()
	n.Sensors = sensors
	n.InsecureSkipVerify = true
	n.Log = noopLogger{}
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return n
}

// sampleReply mimics the body of a NETCONF <get> <rpc-reply> (reply.Data),
// including the <data> wrapper and namespaces.
const sampleReply = `<data>
  <interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces">
    <interface>
      <name>GigabitEthernet0/0</name>
      <statistics><in-octets>100</in-octets><out-octets>200</out-octets></statistics>
    </interface>
    <interface>
      <name>GigabitEthernet0/1</name>
      <statistics><in-octets>300</in-octets><out-octets>400</out-octets></statistics>
    </interface>
  </interfaces>
</data>`

func TestSensorParsesInterfaceStatistics(t *testing.T) {
	n := newInitializedPlugin(t, interfaceSensor())

	metrics, err := n.Sensors[0].parser.Parse([]byte(sampleReply))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(metrics) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(metrics))
	}

	want := []struct {
		iface string
		in    int64
		out   int64
	}{
		{"GigabitEthernet0/0", 100, 200},
		{"GigabitEthernet0/1", 300, 400},
	}
	for i, m := range metrics {
		if m.Name() != "netconf_interface" {
			t.Errorf("metric %d name = %q, want netconf_interface", i, m.Name())
		}
		if got := m.Tags()["interface"]; got != want[i].iface {
			t.Errorf("metric %d interface tag = %q, want %q", i, got, want[i].iface)
		}
		if got := m.Fields()["input_bytes"]; got != want[i].in {
			t.Errorf("metric %d input_bytes = %v (%T), want %d", i, got, got, want[i].in)
		}
		if got := m.Fields()["output_bytes"]; got != want[i].out {
			t.Errorf("metric %d output_bytes = %v (%T), want %d", i, got, got, want[i].out)
		}
	}
}

func TestInitRequiresSensors(t *testing.T) {
	n := NewNetconf()
	n.InsecureSkipVerify = true
	n.Log = noopLogger{}
	if err := n.Init(); err == nil {
		t.Fatal("expected error when no sensors are configured, got nil")
	}
}

func TestInitRejectsEmptyRPC(t *testing.T) {
	n := NewNetconf()
	n.Sensors = []Sensor{{RPC: "   "}}
	n.InsecureSkipVerify = true
	n.Log = noopLogger{}
	if err := n.Init(); err == nil {
		t.Fatal("expected error for empty rpc, got nil")
	}
}

func TestInitDefaultsTimeout(t *testing.T) {
	n := newInitializedPlugin(t, interfaceSensor())
	if n.Timeout != config.Duration(defaultTimeout) {
		t.Errorf("Timeout = %v, want %v", n.Timeout, defaultTimeout)
	}
	if n.hostKeyCallback == nil {
		t.Error("hostKeyCallback is nil after Init")
	}
}

func TestInitKnownHostsMissingFile(t *testing.T) {
	n := NewNetconf()
	n.Sensors = []Sensor{interfaceSensor()}
	n.KnownHostsFile = filepath.Join(t.TempDir(), "does-not-exist")
	n.Log = noopLogger{}
	if err := n.Init(); err == nil {
		t.Fatal("expected error for missing known_hosts file, got nil")
	}
}

func TestInitKnownHostsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("writing known_hosts: %v", err)
	}
	n := NewNetconf()
	n.Sensors = []Sensor{interfaceSensor()}
	n.KnownHostsFile = path
	n.Log = noopLogger{}
	if err := n.Init(); err != nil {
		t.Fatalf("Init with empty known_hosts: %v", err)
	}
	if n.hostKeyCallback == nil {
		t.Error("hostKeyCallback is nil after Init")
	}
}

// TestSensorTOMLDecoding locks the config contract: the embedded xpath.Config
// keys must flatten into the [[sensor]] table (as Telegraf's TOML decoder
// does), rather than requiring a nested sub-table.
func TestSensorTOMLDecoding(t *testing.T) {
	const conf = `
timeout = "5s"

[[devices]]
  address = "10.0.0.1:830"
  username = "admin"
  password = "secret"

[[sensor]]
  rpc = "<get/>"
  metric_name = "'netconf_interface'"
  metric_selection = "//*[local-name()='interface']"
  [sensor.fields_int]
    input_bytes = ".//*[local-name()='in-octets']"
`
	var n Netconf
	if err := itoml.Unmarshal([]byte(conf), &n); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(n.Sensors) != 1 {
		t.Fatalf("expected 1 sensor, got %d", len(n.Sensors))
	}
	s := n.Sensors[0]
	if s.RPC != "<get/>" {
		t.Errorf("rpc = %q, want <get/>", s.RPC)
	}
	if s.Selection != "//*[local-name()='interface']" {
		t.Errorf("metric_selection did not flatten: got %q", s.Selection)
	}
	if s.FieldsInt["input_bytes"] != ".//*[local-name()='in-octets']" {
		t.Errorf("fields_int did not decode: got %v", s.FieldsInt)
	}
	if n.Timeout != config.Duration(5_000_000_000) {
		t.Errorf("timeout = %v, want 5s", n.Timeout)
	}
}
