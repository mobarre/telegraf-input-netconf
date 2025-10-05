package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/influxdata/telegraf/plugins/common/shim"
	_ "github.com/mobarre/telegraf-input-netconf/plugins/inputs/netconf"
)

var pollInterval = flag.Duration("poll_interval", 1*time.Second, "how often to send metrics")

var pollIntervalDisabled = flag.Bool(
	"poll_interval_disabled",
	false,
	"set to true to disable polling. You want to use this when you are sending metrics on your own schedule",
)
var configFile = flag.String("config", "", "path to the config file for this plugin")
var err error

func main() {
	// parse command line options
	flag.Parse()
	if *pollIntervalDisabled {
		*pollInterval = shim.PollIntervalDisabled
	}

	// create the shim. This is what will run your plugins.
	shimLayer := shim.New()

	if err = shimLayer.LoadConfig(configFile); err != nil {
		fmt.Fprintf(os.Stderr, "Err loading input: %s\n", err)
		os.Exit(1)
	}

	// run a single plugin until stdin closes, or we receive a termination signal
	if err = shimLayer.Run(*pollInterval); err != nil {
		fmt.Fprintf(os.Stderr, "Err: %s\n", err)
		os.Exit(1)
	}
}
