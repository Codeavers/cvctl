package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/codeavers/cvctl/internal/render"
)

// Values of instanceStatusItem.Routes.
const (
	routesOK       = "ok"
	routesMismatch = "mismatch"
	routesDown     = "down"
	routesNone     = "-"
)

// Each probe is bounded and all of them share one deadline, so status stays
// fast on a host whose Traefik is down or filtered.
var (
	routeProbeTimeout = 3 * time.Second
	routeCheckTimeout = 5 * time.Second
)

const routeProbeWorkers = 8

var errNoRouter = errors.New("Traefik answered 404 page not found: no router matches this domain")

// RouteProber sends one request for domain to the local Traefik entry point
// of the given edge mode and returns nil if a router answered.
type RouteProber func(ctx context.Context, edge string, proxyPort int, domain string) error

func (e *OpsEnv) traefikDir() string {
	if e.Deployer != nil && e.Deployer.TraefikDir != "" {
		return e.Deployer.TraefikDir
	}
	return "/etc/traefik/dynamic"
}

// checkRoutes compares each instance's route file with the host edge mode and
// probes every plain Host() router through the local Traefik. It returns the
// routes state per instance and one warning per problem.
func (e *OpsEnv) checkRoutes(ctx context.Context, instances []string) (map[string]string, []string) {
	edge := e.edgeMode()
	proxyPort := 0
	if e.Host != nil {
		proxyPort = e.Host.ProxyPort
	}
	probe := e.ProbeRoute
	if probe == nil {
		probe = probeRoute
	}

	res := make(map[string]string, len(instances))
	var warnings []string
	type target struct{ inst, domain string }
	var targets []target
	for _, inst := range instances {
		path := filepath.Join(e.traefikDir(), inst+".yml")
		b, err := e.fs().ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			res[inst] = routesNone
			continue
		}
		if err != nil {
			res[inst] = routesMismatch
			warnings = append(warnings, fmt.Sprintf("%s: cannot read route %s: %v", inst, path, err))
			continue
		}
		rc, err := render.CheckRoute(b, edge)
		if err != nil {
			res[inst] = routesMismatch
			warnings = append(warnings, fmt.Sprintf("%s: cannot parse route %s: %v", inst, path, err))
			continue
		}
		res[inst] = routesOK
		if len(rc.Problems) > 0 {
			res[inst] = routesMismatch
			for _, p := range rc.Problems {
				warnings = append(warnings, fmt.Sprintf("%s: %s in %s; run cvctl deploy %s", inst, p, path, inst))
			}
		}
		for _, r := range rc.Routers {
			if r.Domain != "" {
				targets = append(targets, target{inst, r.Domain})
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, routeCheckTimeout)
	defer cancel()
	errs := make([]error, len(targets))
	sem := make(chan struct{}, routeProbeWorkers)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, pcancel := context.WithTimeout(ctx, routeProbeTimeout)
			defer pcancel()
			errs[i] = probe(pctx, edge, proxyPort, t.domain)
		}()
	}
	wg.Wait()
	for i, t := range targets {
		if errs[i] == nil {
			continue
		}
		if res[t.inst] == routesOK {
			res[t.inst] = routesDown
		}
		warnings = append(warnings, fmt.Sprintf("%s: %s does not respond through Traefik (%s mode): %v", t.inst, t.domain, edge, errs[i]))
	}
	return res, warnings
}

// probeRoute sends GET / for domain to the local Traefik: :443 with SNI in
// edge mode, the loopback proxy port behind a proxy. Any answer other than
// Traefik's own 404 means a router matched.
func probeRoute(ctx context.Context, edge string, proxyPort int, domain string) error {
	scheme, addr := "https", "127.0.0.1:443"
	if edge != "edge" {
		if proxyPort <= 0 {
			proxyPort = 8081
		}
		scheme, addr = "http", fmt.Sprintf("127.0.0.1:%d", proxyPort)
	}
	return probeAddr(ctx, scheme, addr, domain)
}

func probeAddr(ctx context.Context, scheme, addr, domain string) error {
	var d net.Dialer
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		// Reachability only: the certificate may still be pending issuance, and
		// no data is sent or trusted.
		TLSClientConfig:   &tls.Config{ServerName: domain, InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{
		Transport:     tr,
		Timeout:       routeProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+domain+"/", nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode == http.StatusNotFound && string(body) == "404 page not found\n" {
		return errNoRouter
	}
	return nil
}
