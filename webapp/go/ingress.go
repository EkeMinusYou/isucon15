package main

import (
	"context"
	"crypto/hmac"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
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
		a := &App{appCore: &appCore{}}
		return computeHandler(a), a, nil
	}
	if owner {
		constructor := newApp
		if os.Getenv("MASTER_STATE_DIR") != "" {
			constructor = newAppCore
		}
		a, e := constructor(envOr("INITIAL_DIR", "/home/isucon/isureal-initial"), envOr("STATE_DIR", "/home/isucon/isureal-state"))
		if e != nil {
			return nil, nil, e
		}
		if masterDir := os.Getenv("MASTER_STATE_DIR"); masterDir != "" {
			files := &regionalUploads{root: a.durable, region: region}
			master, err := newRegionalMaster(a.initial, masterDir, filepath.Join(a.initial, "images"), region, files)
			if err != nil {
				return nil, nil, err
			}
			if err = a.enableRegionalMasters(master); err != nil {
				master.close()
				return nil, nil, err
			}
			h, err := master.route(a)
			return h, a, err
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
	edge := &App{appCore: &appCore{}}
	var uploads *regionalUploads
	if root := os.Getenv("REGIONAL_STATE_DIR"); root != "" {
		uploads, e = newRegionalUploads(root)
		if e != nil {
			return nil, nil, e
		}
		edge.durable = filepath.Join(root, "pending")
		uploads.region = region
	}
	proxy.Transport = measurement.NewUpstreamTransport()
	imageRoot := os.Getenv("IMAGE_CACHE_DIR")
	base := proxy.Director
	proxy.Director = func(r *http.Request) {
		base(r)
		r.Host = target.Host
		r.Header.Set("X-Country-Code", "JP")
		r.Header.Set("X-ISU-Region", ingressRegion(r))
		r.Header.Set("X-ISU-Source", ingressRegion(r))
		r.Header.Set("X-ISU-Transport", transportSecret)
		r.Header.Del(regionalImageRequest)
		if imageRoot != "" && r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/images/") {
			r.Header.Set(regionalImageRequest, "1")
		}
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if uploads != nil {
			uploads.finish(resp)
		}
		resp.Header.Del(regionalUploadAccepted)
		if err := serveRegionalImage(resp, imageRoot, uploads, proxy.Transport, &edge.regionalImages); err != nil {
			return err
		}
		if identity := resp.Header.Get("X-ISU-Identity"); identity != "" {
			setMeasurementIdentity(resp.Request, identity)
		}
		resp.Header.Del("X-ISU-Identity")
		return nil
	}
	proxy.FlushInterval = -1
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Value(ingressRegionKey{}).(string); !ok {
			r = r.WithContext(context.WithValue(r.Context(), ingressRegionKey{}, region))
		}
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		if uploads != nil && uploads.internal(w, r) {
			return
		}
		r.Header.Del("X-ISU-Transport")
		r.Header.Del("X-ISU-Region")
		r.Header.Del("X-ISU-Source")
		r.Header.Del(regionalUploadHeader)
		if profile, multipart := isImageUpload(r); uploads != nil && (profile || multipart) {
			var p *preparedUpload
			r, p = uploads.prepare(edge, w, r)
			defer uploads.discard(p)
		}
		proxy.ServeHTTP(w, r)
	})
	if masterDir := os.Getenv("MASTER_STATE_DIR"); masterDir != "" {
		master, err := newRegionalMaster(envOr("INITIAL_DIR", "/home/isucon/isureal-initial"), masterDir, imageRoot, region, uploads)
		if err != nil {
			return nil, nil, err
		}
		edge.master = master
		h, err := master.route(handler)
		return h, edge, err
	}
	return handler, edge, nil
}
