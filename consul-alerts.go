// Consul Alerts is a tool to send alerts when checks changes status.
// It is built on top of consul KV, Health, and watch features.
package main

import (
	"fmt"
	"os"
	"syscall"

	"net/http"
	"os/signal"

	"encoding/json"
	"io/ioutil"

	"github.com/AcalephStorage/consul-alerts/consul"
	"github.com/AcalephStorage/consul-alerts/notifier"

	"github.com/Sirupsen/logrus"
	log "github.com/Sirupsen/logrus"
	docopt "github.com/docopt/docopt-go"
)

const version = "Consul Alerts 0.6.0"
const usage = `Consul Alerts.

Usage:
  consul-alerts start [--alert-addr=<addr>] [--consul-scheme=<consulscheme>] [--consul-addr=<consuladdr>] [--consul-dc=<dc>] [--consul-acl-token=<token>] [--watch-checks] [--watch-events] [--log-level=<level>] [--config-file=<file>]
  consul-alerts watch (checks|event) [--alert-addr=<addr>] [--log-level=<level>]
  consul-alerts --help
  consul-alerts --version

Options:
  --consul-acl-token=<token>   The consul ACL token [default: ""].
  --consul-scheme=<scheme>     The scheme to use for connecting to consul [default: http]
  --alert-addr=<addr>          The address for the consul-alert api [default: localhost:9000].
  --consul-addr=<consuladdr>   The consul api address [default: localhost:8500].
  --consul-dc=<dc>             The consul datacenter [default: dc1].
  --log-level=<level>          Set the logging level - valid values are "debug", "info", "warn", and "err" [default: warn].
  --watch-checks               Run check watcher.
  --watch-events               Run event watcher.
  --help                       Show this screen.
  --version                    Show version.
  --config-file=<file>         Path to the configuration file in JSON format

`

type stopable interface {
	stop()
}

var consulClient consul.Consul

var logger *logrus.Logger

func main() {
	var confData map[string]interface{}
	loglevelString := log.InfoLevel.String()
	log.SetLevel(log.InfoLevel)
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})

	args, _ := docopt.Parse(usage, nil, true, version, false)
	if configFile, exists := args["--config-file"].(string); exists {
		file, err := ioutil.ReadFile(configFile)
		if err != nil {
			log.Warn(err)
		}
		err = json.Unmarshal(file, &confData)
		if err != nil {
			log.Warn(err)
		}
		log.Debug("Config data: ", confData)
	}

	if confData["log-level"] != nil {
		loglevelString = confData["log-level"].(string)
	}

	if args["--log-level"].(string) != "" {
		loglevelString = args["--log-level"].(string)
	}

	loglevel, err := log.ParseLevel(loglevelString)
	if err == nil {
		log.SetLevel(loglevel)
	} else {
		log.Infof("Log level not set: %s", err)
	}

	log.Info("parsed opts")
	switch {
	case args["start"].(bool):
		daemonMode(args, confData)
	case args["watch"].(bool):
		watchMode(args)
	}
}

func daemonMode(arguments, confData map[string]interface{}) {
	log.Info("Starting in Daemon mode")
	var loglevelString string
	var consulAclToken string
	var consulScheme string
	var consulAddr string
	var consulDc string

	// Define options before setting in either config file or on command line

	defaultConsulScheme := "http"

	watchChecks := false
	watchEvents := false
	addr := ""

	// This exists check only works for arguments with no default. arguments with defaults will always exist.
	// Because of this the current code overrides command line flags with config file options if set.

	if confData["consul-scheme"] != nil {
		consulScheme = confData["consul-scheme"].(string)
	} else {
		if arguments["--consul-scheme"].(string) == "" {
			consulScheme = defaultConsulScheme
		} else {
			consulScheme = arguments["--consul-scheme"].(string)
		}
	}
	if confData["consul-acl-token"] != nil {
		consulAclToken = confData["consul-acl-token"].(string)
	} else {
		consulAclToken = arguments["--consul-acl-token"].(string)
	}
	if confData["consul-addr"] != nil {
		consulAddr = confData["consul-addr"].(string)
	} else {
		consulAddr = arguments["--consul-addr"].(string)
	}
	if confData["consul-dc"] != nil {
		consulDc = confData["consul-dc"].(string)
	} else {
		consulDc = arguments["--consul-dc"].(string)
	}
	if confData["alert-addr"] != nil {
		addr = confData["alert-addr"].(string)
	} else {
		addr = arguments["--alert-addr"].(string)
	}

	log.Infof("Attempting to start consul-alert daemon on http://%s/", addr)
	url := fmt.Sprintf("http://%s/v1/info", addr)
	resp, err := http.Get(url)
	if err == nil && resp.StatusCode == 201 {
		version := resp.Header.Get("version")
		resp.Body.Close()
		log.Infof("consul-alert daemon already running version: %s", version)
		os.Exit(1)
	}

	log.Infof("Attempting to connect to Consul. Addr: %s://%s DC: %s", consulScheme, consulAddr, consulDc)
	consulClient, err = consul.NewClient(consulScheme, consulAddr, consulDc, consulAclToken, loglevelString)
	if err != nil {
		log.Errorf("Cluster has no leader or is unreacheable: %s", err)
		os.Exit(3)
	}

	hostname, _ := os.Hostname()

	log.Infoln("Consul Alerts daemon started")
	log.Infof("Consul Alerts Host: %s", hostname)
	log.Infof("Consul Agent: %s://%s", consulScheme, consulAddr)
	log.Infof("Consul Datacenter: %s", consulDc)

	leaderCandidate := startLeaderElection(consulScheme, consulAddr, consulDc, consulAclToken)
	notifEngine := startNotifEngine()

	ep := startEventProcessor()
	cp := startCheckProcessor(leaderCandidate, notifEngine)

	http.HandleFunc("/v1/info", infoHandler)
	http.HandleFunc("/v1/process/events", ep.eventHandler)
	http.HandleFunc("/v1/process/checks", cp.checkHandler)
	http.HandleFunc("/v1/health/wildcard", healthWildcardHandler)
	http.HandleFunc("/v1/health", healthHandler)
	go startAPI(addr)

	log.Infoln("Started Consul-Alerts API")

	if watchChecks {
		go runWatcher(consulScheme, consulAddr, consulDc, addr, loglevelString, consulAclToken, "checks")
	}
	if watchEvents {
		go runWatcher(consulScheme, consulAddr, consulDc, addr, loglevelString, consulAclToken, "event")
	}

	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-ch
	cleanup(notifEngine, cp, ep, leaderCandidate)
}

func startAPI(addr string) {
	err := http.ListenAndServe(addr, nil)
	if err != nil {
		log.Infoln("Error starting Consul-Alerts API", err)
		os.Exit(1)
	}
}

func watchMode(arguments map[string]interface{}) {

	checkMode := arguments["checks"].(bool)
	eventMode := arguments["event"].(bool)
	addr := arguments["--alert-addr"].(string)

	var watchType string
	switch {
	case checkMode:
		watchType = "checks"
	case eventMode:
		watchType = "events"
	}

	url := fmt.Sprintf("http://%s/v1/process/%s", addr, watchType)
	resp, err := http.Post(url, "text/json", os.Stdin)
	if err != nil {
		log.Infoln("consul-alert daemon is not running.", err)
		os.Exit(2)
	} else {
		resp.Body.Close()
	}
}

func infoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("version", version)
	w.WriteHeader(201)
}

func cleanup(stopables ...stopable) {
	log.Infoln("Shutting down...")
	for _, s := range stopables {
		s.stop()
	}
}

func builtinNotifiers() map[string]notifier.Notifier {

	emailNotifier := consulClient.EmailNotifier()
	logNotifier := consulClient.LogNotifier()
	influxdbNotifier := consulClient.InfluxdbNotifier()
	slackNotifier := consulClient.SlackNotifier()
	mattermostNotifier := consulClient.MattermostNotifier()
	mattermostWebhookNotifier := consulClient.MattermostWebhookNotifier()
	pagerdutyNotifier := consulClient.PagerDutyNotifier()
	hipchatNotifier := consulClient.HipChatNotifier()
	opsgenieNotifier := consulClient.OpsGenieNotifier()
	awssnsNotifier := consulClient.AwsSnsNotifier()
	victoropsNotifier := consulClient.VictorOpsNotifier()
	ilertNotifier := consulClient.ILertNotifier()

	notifiers := map[string]notifier.Notifier{}
	if emailNotifier.Enabled {
		notifiers[emailNotifier.NotifierName()] = emailNotifier
	}
	if logNotifier.Enabled {
		notifiers[logNotifier.NotifierName()] = logNotifier
	}
	if influxdbNotifier.Enabled {
		notifiers[influxdbNotifier.NotifierName()] = influxdbNotifier
	}
	if slackNotifier.Enabled {
		notifiers[slackNotifier.NotifierName()] = slackNotifier
	}
	if mattermostNotifier.Enabled {
		notifiers[mattermostNotifier.NotifierName()] = mattermostNotifier
	}
	if mattermostWebhookNotifier.Enabled {
		notifiers[mattermostWebhookNotifier.NotifierName()] = mattermostWebhookNotifier
	}
	if pagerdutyNotifier.Enabled {
		notifiers[pagerdutyNotifier.NotifierName()] = pagerdutyNotifier
	}
	if hipchatNotifier.Enabled {
		notifiers[hipchatNotifier.NotifierName()] = hipchatNotifier
	}
	if opsgenieNotifier.Enabled {
		notifiers[opsgenieNotifier.NotifierName()] = opsgenieNotifier
	}
	if awssnsNotifier.Enabled {
		notifiers[awssnsNotifier.NotifierName()] = awssnsNotifier
	}
	if victoropsNotifier.Enabled {
		notifiers[victoropsNotifier.NotifierName()] = victoropsNotifier
	}
	if ilertNotifier.Enabled {
		notifiers[ilertNotifier.NotifierName()] = ilertNotifier
	}

	return notifiers
}
