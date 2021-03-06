// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Relaynode is the old Linux Tailscale daemon.
//
// Deprecated: this program will be soon deleted. The replacement is
// cmd/tailscaled.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/apenwarr/fixconsole"
	"github.com/google/go-cmp/cmp"
	"github.com/klauspost/compress/zstd"
	"github.com/pborman/getopt/v2"
	"github.com/tailscale/wireguard-go/wgcfg"
	"tailscale.com/atomicfile"
	"tailscale.com/control/controlclient"
	"tailscale.com/control/policy"
	"tailscale.com/logpolicy"
	"tailscale.com/version"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/filter"
	"tailscale.com/wgengine/magicsock"
)

func main() {
	err := fixconsole.FixConsoleIfNeeded()
	if err != nil {
		log.Printf("fixConsoleOutput: %v\n", err)
	}
	config := getopt.StringLong("config", 'f', "", "path to config file")
	server := getopt.StringLong("server", 's', "https://login.tailscale.com", "URL to tailcontrol server")
	listenport := getopt.Uint16Long("port", 'p', magicsock.DefaultPort, "WireGuard port (0=autoselect)")
	tunname := getopt.StringLong("tun", 0, "wg0", "tunnel interface name")
	alwaysrefresh := getopt.BoolLong("always-refresh", 0, "force key refresh at startup")
	fake := getopt.BoolLong("fake", 0, "fake tunnel+routing instead of tuntap")
	nuroutes := getopt.BoolLong("no-single-routes", 'N', "disallow (non-subnet) routes to single nodes")
	rroutes := getopt.BoolLong("remote-routes", 'R', "allow routing subnets to remote nodes")
	droutes := getopt.BoolLong("default-routes", 'D', "allow default route on remote node")
	routes := getopt.StringLong("routes", 0, "", "list of IP ranges this node can relay")
	aclfile := getopt.StringLong("acl-file", 0, "", "restrict traffic relaying according to json ACL file")
	derp := getopt.BoolLong("derp", 0, "enable bypass via Detour Encrypted Routing Protocol (DERP)", "false")
	debug := getopt.StringLong("debug", 0, "", "Address of debug server")
	getopt.Parse()
	if len(getopt.Args()) > 0 {
		log.Fatalf("too many non-flag arguments: %#v", getopt.Args()[0])
	}
	uflags := controlclient.UFlagsHelper(!*nuroutes, *rroutes, *droutes)
	if *config == "" {
		log.Fatal("no --config file specified")
	}
	if *tunname == "" {
		log.Printf("Warning: no --tun device specified; routing disabled.\n")
	}

	pol := logpolicy.New("tailnode.log.tailscale.io", *config)

	logf := wgengine.RusagePrefixLog(log.Printf)

	// The wgengine takes a wireguard configuration produced by the
	// controlclient, and runs the actual tunnels and packets.
	var e wgengine.Engine
	if *fake {
		e, err = wgengine.NewFakeUserspaceEngine(logf, *listenport, *derp)
	} else {
		e, err = wgengine.NewUserspaceEngine(logf, *tunname, *listenport, *derp)
	}
	if err != nil {
		log.Fatalf("Error starting wireguard engine: %v\n", err)
	}

	e = wgengine.NewWatchdog(e)
	var lastacljson string
	var p *policy.Policy

	if *aclfile == "" {
		e.SetFilter(nil)
	} else {
		lastacljson = readOrFatal(*aclfile)
		p = installFilterOrFatal(e, *aclfile, lastacljson, nil)
	}

	var lastNetMap *controlclient.NetworkMap
	var lastUserMap map[string][]filter.IP
	statusFunc := func(new controlclient.Status) {
		if new.URL != "" {
			fmt.Fprintf(os.Stderr, "To authenticate, visit:\n\n\t%s\n\n", new.URL)
			return
		}
		if new.Err != "" {
			log.Print(new.Err)
			return
		}
		if new.Persist != nil {
			if err := saveConfig(*config, *new.Persist); err != nil {
				log.Println(err)
			}
		}

		if m := new.NetMap; m != nil {
			if lastNetMap != nil {
				s1 := strings.Split(lastNetMap.Concise(), "\n")
				s2 := strings.Split(new.NetMap.Concise(), "\n")
				logf("netmap diff:\n%v\n", cmp.Diff(s1, s2))
			}
			lastNetMap = m

			if m.Equal(&controlclient.NetworkMap{}) {
				return
			}

			wgcfg, err := m.WGCfg(uflags, m.DNS)
			if err != nil {
				log.Fatalf("Error getting wg config: %v\n", err)
			}
			err = e.Reconfig(wgcfg, m.DNSDomains)
			if err != nil {
				log.Fatalf("Error reconfiguring engine: %v\n", err)
			}
			lastUserMap = m.UserMap()
			if p != nil {
				matches, err := p.Expand(lastUserMap)
				if err != nil {
					log.Fatalf("Error expanding ACLs: %v\n", err)
				}
				e.SetFilter(filter.New(matches))
			}
		}
	}

	cfg, err := loadConfig(*config)
	if err != nil {
		log.Fatal(err)
	}

	hi := controlclient.NewHostinfo()
	hi.FrontendLogID = pol.PublicID.String()
	hi.BackendLogID = pol.PublicID.String()
	if *routes != "" {
		for _, routeStr := range strings.Split(*routes, ",") {
			cidr, err := wgcfg.ParseCIDR(routeStr)
			if err != nil {
				log.Fatalf("--routes: not an IP range: %s", routeStr)
			}
			hi.RoutableIPs = append(hi.RoutableIPs, *cidr)
		}
	}

	c, err := controlclient.New(controlclient.Options{
		Persist:   cfg,
		ServerURL: *server,
		Hostinfo:  &hi,
		NewDecompressor: func() (controlclient.Decompressor, error) {
			return zstd.NewReader(nil)
		},
		KeepAlive: true,
	})
	c.SetStatusFunc(statusFunc)
	if err != nil {
		log.Fatal(err)
	}
	lf := controlclient.LoginDefault
	if *alwaysrefresh {
		lf |= controlclient.LoginInteractive
	}
	c.Login(nil, lf)

	// Print the wireguard status when we get an update.
	e.SetStatusCallback(func(s *wgengine.Status, err error) {
		if err != nil {
			log.Fatalf("Wireguard engine status error: %v\n", err)
		}
		var ss []string
		for _, p := range s.Peers {
			if p.LastHandshake.IsZero() {
				ss = append(ss, "x")
			} else {
				ss = append(ss, fmt.Sprintf("%d/%d", p.RxBytes, p.TxBytes))
			}
		}
		logf("v%v peers: %v\n", version.LONG, strings.Join(ss, " "))
		c.UpdateEndpoints(0, s.LocalAddrs)
	})

	if *debug != "" {
		go runDebugServer(*debug)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	signal.Notify(sigCh, syscall.SIGTERM)

	t := time.NewTicker(5 * time.Second)
loop:
	for {
		select {
		case <-t.C:
			// For the sake of curiosity, request a status
			// update periodically.
			e.RequestStatus()

			// check if aclfile has changed.
			// TODO(apenwarr): use fsnotify instead of polling?
			if *aclfile != "" {
				json := readOrFatal(*aclfile)
				if json != lastacljson {
					logf("ACL file (%v) changed. Reloading filter.\n", *aclfile)
					lastacljson = json
					p = installFilterOrFatal(e, *aclfile, json, lastUserMap)
				}
			}
		case <-sigCh:
			logf("signal received, exiting")
			t.Stop()
			break loop
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	e.Close()
	pol.Shutdown(ctx)
}

func loadConfig(path string) (cfg controlclient.Persist, err error) {
	b, err := ioutil.ReadFile(path)
	if os.IsNotExist(err) {
		log.Printf("config %s does not exist", path)
		return controlclient.Persist{}, nil
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return controlclient.Persist{}, fmt.Errorf("load config: %v", err)
	}
	return cfg, nil
}

func saveConfig(path string, cfg controlclient.Persist) error {
	b, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return fmt.Errorf("save config: %v", err)
	}
	if err := atomicfile.WriteFile(path, b, 0666); err != nil {
		return fmt.Errorf("save config: %v", err)
	}
	return nil
}

func readOrFatal(filename string) string {
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Fatalf("%v: ReadFile: %v\n", filename, err)
	}
	return string(b)
}

func installFilterOrFatal(e wgengine.Engine, filename, acljson string, usermap map[string][]filter.IP) *policy.Policy {
	p, err := policy.Parse(acljson)
	if err != nil {
		log.Fatalf("%v: json filter: %v\n", filename, err)
	}

	matches, err := p.Expand(usermap)
	if err != nil {
		log.Fatalf("%v: json filter: %v\n", filename, err)
	}

	e.SetFilter(filter.New(matches))
	return p
}

func runDebugServer(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := http.Server{
		Addr:    addr,
		Handler: mux,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
