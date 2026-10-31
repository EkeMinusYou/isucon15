package main

import (
	"crypto/hmac"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	measurement "github.com/EkeMinusYou/isucon-template/tools/measurement/app"
)

var fixedRegion = "asia"
var transportSecret string

func isInternal(r *http.Request) bool {
	return transportSecret != "" && hmac.Equal([]byte(r.Header.Get("X-ISU-Transport")), []byte(transportSecret))
}
func detectRole() (string, bool, error) {
	if x := os.Getenv("REGION"); x != "" {
		return x, os.Getenv("OWNER_URL") == "", nil
	}
	addrs, e := net.InterfaceAddrs()
	if e != nil {
		return "", false, e
	}
	for _, a := range addrs {
		ip, _, _ := net.ParseCIDR(a.String())
		if ip == nil {
			continue
		}
		switch ip.String() {
		case "192.168.1.10":
			return "asia", true, nil
		case "192.168.2.10":
			return "europe", false, nil
		case "192.168.3.10":
			return "america", false, nil
		case "192.168.4.10":
			return "compute", false, nil
		}
	}
	return "", false, fmt.Errorf("host has no configured contest interface")
}
func serviceHandler() (http.Handler, *App, error) {
	region, owner, e := detectRole()
	if e != nil {
		return nil, nil, e
	}
	fixedRegion = region
	key, e := os.ReadFile(envOr("TRANSPORT_KEY_FILE", "/home/isucon/isureal-transport.key"))
	if e != nil {
		return nil, nil, e
	}
	transportSecret = strings.TrimSpace(string(key))
	if transportSecret == "" {
		return nil, nil, fmt.Errorf("empty transport key")
	}
	log.Printf("region=%s state_owner=%v", region, owner)
	if region == "compute" {
		a := &App{signatures: map[string]string{}}
		return computeHandler(a), a, nil
	}
	if owner {
		a, e := newApp(envOr("INITIAL_DIR", "/home/isucon/isureal-initial"), envOr("STATE_DIR", "/home/isucon/isureal-state"))
		if e != nil {
			return nil, nil, e
		}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isInternal(r) {
				r.Header.Del("X-ISU-Region")
				r.Header.Del("X-ISU-Source")
			} else if !validRegion(r.Header.Get("X-ISU-Region")) {
				failure(w, 400, "invalid_region")
				return
			}
			a.ServeHTTP(w, r)
		})
		return h, a, nil
	}
	target, e := url.Parse(os.Getenv("OWNER_URL"))
	if e != nil || target.Scheme != "http" || target.Host == "" {
		return nil, nil, fmt.Errorf("OWNER_URL must name the owner through the provided network route")
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = measurement.NewUpstreamTransport()
	base := proxy.Director
	proxy.Director = func(r *http.Request) {
		base(r)
		r.Host = target.Host
		r.Header.Set("X-Country-Code", "JP")
		r.Header.Set("X-ISU-Region", region)
		r.Header.Set("X-ISU-Source", region)
		r.Header.Set("X-ISU-Transport", transportSecret)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if identity := resp.Header.Get("X-ISU-Identity"); identity != "" {
			setMeasurementIdentity(resp.Request, identity)
		}
		resp.Header.Del("X-ISU-Identity")
		return nil
	}
	proxy.FlushInterval = -1
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		r.Header.Del("X-ISU-Transport")
		r.Header.Del("X-ISU-Region")
		r.Header.Del("X-ISU-Source")
		proxy.ServeHTTP(w, r)
	}), nil, nil
}
