// Command smlossproxy runs the lost-reply proxy (tools/lossproxy) in front of
// the Odoo sandbox, for Sindhu's QA and manual failure tests. Both addresses
// must be loopback; it is for the synthetic sandbox only.
//
//	smlossproxy -target http://127.0.0.1:18069 -listen 127.0.0.1:18070 -control 127.0.0.1:18071
//
// Point the connector at the -listen address, then add faults:
//
//	curl -X POST 127.0.0.1:18071/faults -d '{"mode":"drop_reply","method":"sm_create","op_ref":"op-1"}'
//	curl 127.0.0.1:18071/events?op_ref=op-1
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/tools/lossproxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "smlossproxy:", err)
		os.Exit(1)
	}
}

func run() error {
	target := flag.String("target", "http://127.0.0.1:18069", "Odoo base URL")
	listen := flag.String("listen", "127.0.0.1:18070", "data address the connector uses (loopback)")
	control := flag.String("control", "127.0.0.1:18071", "fault-control address (loopback)")
	flag.Parse()
	for _, a := range []string{*listen, *control} {
		if !loopback(a) {
			return fmt.Errorf("%s is not a loopback address; the proxy is for the local sandbox only", a)
		}
	}
	p, err := lossproxy.New(*target)
	if err != nil {
		return err
	}
	errc := make(chan error, 2)
	serve := func(addr string, h http.Handler) {
		srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		errc <- srv.ListenAndServe()
	}
	go serve(*listen, p)
	go serve(*control, p.Control())
	slog.Info("lossproxy running", "data", *listen, "control", *control, "target", *target)
	return <-errc
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
