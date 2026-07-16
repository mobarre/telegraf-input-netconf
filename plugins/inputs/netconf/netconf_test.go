// Copyright (C) 2025 Marc-Olivier Barre
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. See the LICENSE file for details.

package netconf

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Juniper/go-netconf/netconf"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/parsers/xpath"
	itoml "github.com/influxdata/toml"
	"golang.org/x/crypto/ssh"
)

// --- test doubles -----------------------------------------------------------

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

// fakeSession is an in-memory ncSession.
type fakeSession struct {
	reply   string
	execErr error
	execN   int32
	closed  int32
}

func (s *fakeSession) Exec(...netconf.RPCMethod) (*netconf.RPCReply, error) {
	atomic.AddInt32(&s.execN, 1)
	if s.execErr != nil {
		return nil, s.execErr
	}
	return &netconf.RPCReply{Data: s.reply}, nil
}
func (s *fakeSession) Close() error     { atomic.StoreInt32(&s.closed, 1); return nil }
func (s *fakeSession) isClosed() bool   { return atomic.LoadInt32(&s.closed) == 1 }
func (s *fakeSession) execCount() int32 { return atomic.LoadInt32(&s.execN) }

// fakeDialer records dials, tracks peak concurrency, and hands out fakeSessions.
type fakeDialer struct {
	mu       sync.Mutex
	dialed   []string
	sessions []*fakeSession
	reply    string
	execErr  error
	delay    time.Duration
	curConc  int32
	maxConc  int32
}

func (d *fakeDialer) fn(device Device, _ *ssh.ClientConfig, _ time.Duration) (ncSession, error) {
	c := atomic.AddInt32(&d.curConc, 1)
	for {
		m := atomic.LoadInt32(&d.maxConc)
		if c <= m || atomic.CompareAndSwapInt32(&d.maxConc, m, c) {
			break
		}
	}
	if d.delay > 0 {
		time.Sleep(d.delay)
	}
	atomic.AddInt32(&d.curConc, -1)

	s := &fakeSession{reply: d.reply, execErr: d.execErr}
	d.mu.Lock()
	d.dialed = append(d.dialed, device.Address)
	d.sessions = append(d.sessions, s)
	d.mu.Unlock()
	return s, nil
}

func (d *fakeDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.dialed)
}

// testAccumulator captures metrics and errors; other methods are no-ops.
type testAccumulator struct {
	mu      sync.Mutex
	metrics []telegraf.Metric
	errs    []error
}

func (a *testAccumulator) AddMetric(m telegraf.Metric) {
	a.mu.Lock()
	a.metrics = append(a.metrics, m)
	a.mu.Unlock()
}
func (a *testAccumulator) AddError(err error) {
	a.mu.Lock()
	a.errs = append(a.errs, err)
	a.mu.Unlock()
}
func (a *testAccumulator) metricCount() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.metrics) }
func (a *testAccumulator) errorCount() int  { a.mu.Lock(); defer a.mu.Unlock(); return len(a.errs) }

func (*testAccumulator) AddFields(string, map[string]interface{}, map[string]string, ...time.Time)  {}
func (*testAccumulator) AddGauge(string, map[string]interface{}, map[string]string, ...time.Time)   {}
func (*testAccumulator) AddCounter(string, map[string]interface{}, map[string]string, ...time.Time) {}
func (*testAccumulator) AddSummary(string, map[string]interface{}, map[string]string, ...time.Time) {}
func (*testAccumulator) AddHistogram(string, map[string]interface{}, map[string]string, ...time.Time) {
}
func (*testAccumulator) SetPrecision(time.Duration)                    {}
func (*testAccumulator) WithTracking(int) telegraf.TrackingAccumulator { return nil }

// --- helpers ----------------------------------------------------------------

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

// newPlugin returns an un-Init'd plugin with a sensor, host-key checking off,
// and a test logger. Callers set extra fields then call Init.
func newPlugin(devices ...Device) *Netconf {
	n := NewNetconf()
	n.Devices = devices
	n.Sensors = []Sensor{interfaceSensor()}
	n.InsecureSkipVerify = true
	n.Log = noopLogger{}
	return n
}

func dev(addr string) Device { return Device{Address: addr, Username: "admin"} }

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

// mixedReply pairs a counter-bearing interface with a bare one (an SRv6
// locator that carries only a name). The bare one yields a metric with tags
// but no fields — the case that used to crash the execd shim.
const mixedReply = `<data>
  <interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces">
    <interface>
      <name>GigabitEthernet0/0</name>
      <statistics><in-octets>100</in-octets><out-octets>200</out-octets></statistics>
    </interface>
    <interface>
      <name>srv6-MAIN</name>
    </interface>
  </interfaces>
</data>`

// --- parser -----------------------------------------------------------------

func TestSensorParsesInterfaceStatistics(t *testing.T) {
	n := newPlugin(dev("r1:830"))
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	metrics, err := n.Sensors[0].parser.Parse([]byte(sampleReply))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(metrics) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(metrics))
	}
	want := []struct {
		iface   string
		in, out int64
	}{
		{"GigabitEthernet0/0", 100, 200},
		{"GigabitEthernet0/1", 300, 400},
	}
	for i, m := range metrics {
		if m.Name() != "netconf_interface" {
			t.Errorf("metric %d name = %q", i, m.Name())
		}
		if got := m.Tags()["interface"]; got != want[i].iface {
			t.Errorf("metric %d interface = %q, want %q", i, got, want[i].iface)
		}
		if got := m.Fields()["input_bytes"]; got != want[i].in {
			t.Errorf("metric %d input_bytes = %v, want %d", i, got, want[i].in)
		}
		if got := m.Fields()["output_bytes"]; got != want[i].out {
			t.Errorf("metric %d output_bytes = %v, want %d", i, got, want[i].out)
		}
	}
}

// --- Init validation --------------------------------------------------------

func TestInitRequiresDevices(t *testing.T) {
	n := NewNetconf()
	n.Sensors = []Sensor{interfaceSensor()}
	n.InsecureSkipVerify = true
	n.Log = noopLogger{}
	if err := n.Init(); err == nil {
		t.Fatal("expected error when no devices are configured")
	}
}

func TestInitRequiresSensors(t *testing.T) {
	n := NewNetconf()
	n.Devices = []Device{dev("r1:830")}
	n.InsecureSkipVerify = true
	n.Log = noopLogger{}
	if err := n.Init(); err == nil {
		t.Fatal("expected error when no sensors are configured")
	}
}

func TestInitRejectsEmptyRPC(t *testing.T) {
	n := newPlugin(dev("r1:830"))
	n.Sensors = []Sensor{{RPC: "   "}}
	if err := n.Init(); err == nil {
		t.Fatal("expected error for empty rpc")
	}
}

func TestInitDefaultsApplied(t *testing.T) {
	n := newPlugin(dev("r1:830"))
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n.Timeout != config.Duration(defaultTimeout) {
		t.Errorf("Timeout = %v, want %v", n.Timeout, defaultTimeout)
	}
	if n.SessionMaxAge != config.Duration(defaultSessionMaxAge) {
		t.Errorf("SessionMaxAge = %v, want %v", n.SessionMaxAge, defaultSessionMaxAge)
	}
	if n.MaxConcurrentDevices != 0 { // unset stays 0 = unlimited
		t.Errorf("MaxConcurrentDevices = %d, want 0", n.MaxConcurrentDevices)
	}
	if n.hostKeyCallback == nil {
		t.Error("hostKeyCallback is nil after Init")
	}
}

func TestInitKnownHostsRequiredWhenNotInsecure(t *testing.T) {
	n := NewNetconf()
	n.Devices = []Device{dev("r1:830")}
	n.Sensors = []Sensor{interfaceSensor()}
	n.Log = noopLogger{}
	// Neither known_hosts_file nor insecure_skip_verify set.
	if err := n.Init(); err == nil {
		t.Fatal("expected error requiring known_hosts_file")
	}
}

func TestInitKnownHostsMissingFile(t *testing.T) {
	n := NewNetconf()
	n.Devices = []Device{dev("r1:830")}
	n.Sensors = []Sensor{interfaceSensor()}
	n.Log = noopLogger{}
	n.KnownHostsFile = filepath.Join(t.TempDir(), "does-not-exist")
	if err := n.Init(); err == nil {
		t.Fatal("expected error for missing known_hosts file")
	}
}

func TestInitKnownHostsEmptyFileOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	n := NewNetconf()
	n.Devices = []Device{dev("r1:830")}
	n.Sensors = []Sensor{interfaceSensor()}
	n.Log = noopLogger{}
	n.KnownHostsFile = path
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n.hostKeyCallback == nil {
		t.Error("hostKeyCallback is nil")
	}
}

// --- devices_directory ------------------------------------------------------

func TestDevicesDirectoryMerge(t *testing.T) {
	dir := t.TempDir()
	file := `
[[devices]]
  address = "r10:830"
  username = "admin"
  password = "s3cret"
[[devices]]
  address = "r11:830"
  username = "admin"
  password = "hunter2"
`
	if err := os.WriteFile(filepath.Join(dir, "batch.toml"), []byte(file), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A non-toml file must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignore me"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	n := newPlugin(dev("r1:830")) // one inline device
	n.DevicesDirectory = dir
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if len(n.Devices) != 3 {
		t.Fatalf("expected 3 devices (1 inline + 2 from dir), got %d", len(n.Devices))
	}

	// The password from a device file must decode into a usable Secret.
	var found bool
	for _, d := range n.Devices {
		if d.Address == "r10:830" {
			found = true
			buf, err := d.Password.Get()
			if err != nil {
				t.Fatalf("Password.Get: %v", err)
			}
			if got := buf.TemporaryString(); got != "s3cret" {
				t.Errorf("password = %q, want s3cret", got)
			}
			buf.Destroy()
		}
	}
	if !found {
		t.Fatal("device r10:830 from directory not merged")
	}
}

// --- Gather / connection manager (fake dialer) ------------------------------

func TestGatherReusesSession(t *testing.T) {
	d := &fakeDialer{reply: sampleReply}
	n := newPlugin(dev("r1:830"))
	n.dial = d.fn
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	acc := &testAccumulator{}
	n.Gather(acc)
	n.Gather(acc)

	if d.dialCount() != 1 {
		t.Errorf("dialed %d times, want 1 (session should be reused)", d.dialCount())
	}
	if got := d.sessions[0].execCount(); got != 2 {
		t.Errorf("Exec called %d times, want 2 (one per gather)", got)
	}
	if acc.errorCount() != 0 {
		t.Errorf("unexpected errors: %v", acc.errs)
	}
}

func TestGatherRecyclesAgedSession(t *testing.T) {
	d := &fakeDialer{reply: sampleReply}
	n := newPlugin(dev("r1:830"))
	n.dial = d.fn
	n.SessionMaxAge = config.Duration(time.Nanosecond) // everything is "aged"
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	acc := &testAccumulator{}
	n.Gather(acc)
	time.Sleep(time.Millisecond)
	n.Gather(acc)

	if d.dialCount() != 2 {
		t.Errorf("dialed %d times, want 2 (aged session should be recycled)", d.dialCount())
	}
	if !d.sessions[0].isClosed() {
		t.Error("recycled session was not closed")
	}
}

func TestGatherDropsSessionOnRPCError(t *testing.T) {
	d := &fakeDialer{execErr: errors.New("boom")}
	n := newPlugin(dev("r1:830"))
	n.dial = d.fn
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	acc := &testAccumulator{}
	n.Gather(acc)
	n.Gather(acc)

	if d.dialCount() != 2 {
		t.Errorf("dialed %d times, want 2 (failed session should be dropped and redialed)", d.dialCount())
	}
	if !d.sessions[0].isClosed() {
		t.Error("failed session was not closed")
	}
	if acc.errorCount() != 2 {
		t.Errorf("got %d errors, want 2", acc.errorCount())
	}
}

func TestGatherConcurrencyBounded(t *testing.T) {
	d := &fakeDialer{reply: sampleReply, delay: 20 * time.Millisecond}
	devices := make([]Device, 0, 8)
	for _, a := range []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8"} {
		devices = append(devices, dev(a+":830"))
	}
	n := newPlugin(devices...)
	n.dial = d.fn
	n.MaxConcurrentDevices = 2
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	n.Gather(&testAccumulator{})

	if d.dialCount() != 8 {
		t.Errorf("dialed %d devices, want 8", d.dialCount())
	}
	if peak := atomic.LoadInt32(&d.maxConc); peak > 2 {
		t.Errorf("peak concurrency = %d, want <= 2", peak)
	}
}

func TestGatherTagsMetricsWithDevice(t *testing.T) {
	d := &fakeDialer{reply: sampleReply}
	n := newPlugin(dev("r1:830"))
	n.dial = d.fn
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	acc := &testAccumulator{}
	n.Gather(acc)

	if acc.metricCount() != 2 {
		t.Fatalf("got %d metrics, want 2", acc.metricCount())
	}
	for _, m := range acc.metrics {
		if got := m.Tags()["device"]; got != "r1:830" {
			t.Errorf("device tag = %q, want r1:830", got)
		}
	}
}

// TestGatherDropsFieldlessMetrics guards the fix for the execd crash: a
// selection can match nodes that carry none of the configured fields (e.g. an
// SRv6 locator). Such tag-only metrics must never reach the accumulator, or the
// influx serializer rejects them and the shim tears down the process.
func TestGatherDropsFieldlessMetrics(t *testing.T) {
	d := &fakeDialer{reply: mixedReply}
	n := newPlugin(dev("r1:830"))
	n.dial = d.fn
	if err := n.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	acc := &testAccumulator{}
	n.Gather(acc)

	if acc.metricCount() != 1 {
		t.Fatalf("got %d metrics, want 1 (field-less interface must be dropped)", acc.metricCount())
	}
	if got := acc.metrics[0].Tags()["interface"]; got != "GigabitEthernet0/0" {
		t.Errorf("surviving metric interface = %q, want GigabitEthernet0/0", got)
	}
	if acc.errorCount() != 0 {
		t.Errorf("unexpected errors: %v", acc.errs)
	}
}

// --- config contract --------------------------------------------------------

// TestSensorTOMLDecoding locks the config contract: the embedded xpath.Config
// keys must flatten into the [[sensor]] table rather than requiring a sub-table.
func TestSensorTOMLDecoding(t *testing.T) {
	const conf = `
timeout = "5s"

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
		t.Errorf("rpc = %q", s.RPC)
	}
	if s.Selection != "//*[local-name()='interface']" {
		t.Errorf("metric_selection did not flatten: %q", s.Selection)
	}
	if s.FieldsInt["input_bytes"] != ".//*[local-name()='in-octets']" {
		t.Errorf("fields_int did not decode: %v", s.FieldsInt)
	}
	if n.Timeout != config.Duration(5*time.Second) {
		t.Errorf("timeout = %v, want 5s", n.Timeout)
	}
}
