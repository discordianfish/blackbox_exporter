// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sync"

	"gopkg.in/yaml.v2"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
)

type Config struct {
	Modules map[string]Module `yaml:"modules"`
}

type SafeConfig struct {
	sync.RWMutex
	C *Config
}

type Module struct {
	Prober  string        `yaml:"prober"`
	Timeout time.Duration `yaml:"timeout"`
	HTTP    HTTPProbe     `yaml:"http"`
	TCP     TCPProbe      `yaml:"tcp"`
	ICMP    ICMPProbe     `yaml:"icmp"`
	DNS     DNSProbe      `yaml:"dns"`
}

type HTTPProbe struct {
	// Defaults to 2xx.
	ValidStatusCodes       []int             `yaml:"valid_status_codes"`
	NoFollowRedirects      bool              `yaml:"no_follow_redirects"`
	FailIfSSL              bool              `yaml:"fail_if_ssl"`
	FailIfNotSSL           bool              `yaml:"fail_if_not_ssl"`
	Method                 string            `yaml:"method"`
	Headers                map[string]string `yaml:"headers"`
	FailIfMatchesRegexp    []string          `yaml:"fail_if_matches_regexp"`
	FailIfNotMatchesRegexp []string          `yaml:"fail_if_not_matches_regexp"`
	TLSConfig              config.TLSConfig  `yaml:"tls_config"`
	Protocol               string            `yaml:"protocol"`              // Defaults to "tcp".
	PreferredIPProtocol    string            `yaml:"preferred_ip_protocol"` // Defaults to "ip6".
	Body                   string            `yaml:"body"`
}

type QueryResponse struct {
	Expect string `yaml:"expect"`
	Send   string `yaml:"send"`
}

type TCPProbe struct {
	QueryResponse       []QueryResponse  `yaml:"query_response"`
	TLS                 bool             `yaml:"tls"`
	TLSConfig           config.TLSConfig `yaml:"tls_config"`
	Protocol            string           `yaml:"protocol"`              // Defaults to "tcp".
	PreferredIPProtocol string           `yaml:"preferred_ip_protocol"` // Defaults to "ip6".
}

type ICMPProbe struct {
	Protocol            string `yaml:"protocol"`              // Defaults to "icmp4".
	PreferredIPProtocol string `yaml:"preferred_ip_protocol"` // Defaults to "ip6".
}

type DNSProbe struct {
	Protocol            string         `yaml:"protocol"` // Defaults to "udp".
	QueryName           string         `yaml:"query_name"`
	QueryType           string         `yaml:"query_type"`   // Defaults to ANY.
	ValidRcodes         []string       `yaml:"valid_rcodes"` // Defaults to NOERROR.
	ValidateAnswer      DNSRRValidator `yaml:"validate_answer_rrs"`
	ValidateAuthority   DNSRRValidator `yaml:"validate_authority_rrs"`
	ValidateAdditional  DNSRRValidator `yaml:"validate_additional_rrs"`
	PreferredIPProtocol string         `yaml:"preferred_ip_protocol"` // Defaults to "ip6".
}

type DNSRRValidator struct {
	FailIfMatchesRegexp    []string `yaml:"fail_if_matches_regexp"`
	FailIfNotMatchesRegexp []string `yaml:"fail_if_not_matches_regexp"`
}

type Account interface {
	Book(r *http.Request) error
}

var Probers = map[string]func(string, http.ResponseWriter, Module) bool{
	"http": probeHTTP,
	"tcp":  probeTCP,
	"icmp": probeICMP,
	"dns":  probeDNS,
}

func (sc *SafeConfig) reloadConfig(confFile string) (err error) {
	var c = &Config{}

	yamlFile, err := ioutil.ReadFile(confFile)
	if err != nil {
		log.Errorf("Error reading config file: %s", err)
		return err
	}

	if err := yaml.Unmarshal(yamlFile, c); err != nil {
		log.Errorf("Error parsing config file: %s", err)
		return err
	}

	sc.Lock()
	sc.C = c
	sc.Unlock()

	log.Infoln("Loaded config file")
	return nil
}

func probeHandler(w http.ResponseWriter, r *http.Request, conf *Config, account Account) {
	params := r.URL.Query()
	target := params.Get("target")
	if target == "" {
		http.Error(w, "Target parameter is missing", 400)
		return
	}

	moduleName := params.Get("module")
	if moduleName == "" {
		moduleName = "http_2xx"
	}
	module, ok := conf.Modules[moduleName]
	if !ok {
		http.Error(w, fmt.Sprintf("Unknown module %q", moduleName), 400)
		return
	}
	prober, ok := Probers[module.Prober]
	if !ok {
		http.Error(w, fmt.Sprintf("Unknown prober %q", module.Prober), 400)
		return
	}
	if err := account.Book(r); err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	start := time.Now()
	success := prober(target, w, module)
	fmt.Fprintf(w, "probe_duration_seconds %f\n", time.Since(start).Seconds())
	if success {
		fmt.Fprintln(w, "probe_success 1")
	} else {
		fmt.Fprintln(w, "probe_success 0")
	}
}

func init() {
	prometheus.MustRegister(version.NewCollector("blackbox_exporter"))
}

func main() {
	var (
		configFile    = flag.String("config.file", "blackbox.yml", "Blackbox exporter configuration file.")
		listenAddress = flag.String("web.listen-address", ":9115", "The address to listen on for HTTP requests.")
		showVersion   = flag.Bool("version", false, "Print version information.")
		sc            = &SafeConfig{
			C: &Config{},
		}
	)
	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, version.Print("blackbox_exporter"))
		os.Exit(0)
	}

	log.Infoln("Starting blackbox_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	if err := sc.reloadConfig(*configFile); err != nil {
		log.Fatalf("Error loading config: %s", err)
	}

	hup := make(chan os.Signal)
	reloadCh := make(chan chan error)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-hup:
				if err := sc.reloadConfig(*configFile); err != nil {
					log.Errorf("Error reloading config: %s", err)
				}
			case rc := <-reloadCh:
				if err := sc.reloadConfig(*configFile); err != nil {
					log.Errorf("Error reloading config: %s", err)
					rc <- err
				} else {
					rc <- nil
				}
			}
		}
	}()
	account := &accountNoop{}
	http.Handle("/metrics", prometheus.Handler())
	http.HandleFunc("/probe",
		func(w http.ResponseWriter, r *http.Request) {
			sc.RLock()
			c := sc.C
			sc.RUnlock()

			probeHandler(w, r, c, account)
		})
	http.HandleFunc("/-/reload",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				fmt.Fprintf(w, "This endpoint requires a POST request.\n")
				return
			}

			rc := make(chan error)
			reloadCh <- rc
			if err := <-rc; err != nil {
				http.Error(w, fmt.Sprintf("failed to reload config: %s", err), http.StatusInternalServerError)
			}
		})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
            <head><title>Blackbox Exporter</title></head>
            <body>
            <h1>Blackbox Exporter</h1>
            <p><a href="/probe?target=prometheus.io&module=http_2xx">Probe prometheus.io for http_2xx</a></p>
            <p><a href="/metrics">Metrics</a></p>
            </body>
            </html>`))
	})

	log.Infoln("Listening on", *listenAddress)
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		log.Fatalf("Error starting HTTP server: %s", err)
	}
}
