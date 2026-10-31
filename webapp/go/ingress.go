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
	primary, e := url.Parse(os.Getenv("OWNER_URL"))
	if e != nil {
		return "", false, e
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
			return "asia", primary.Hostname() == "" || primary.Hostname() == ip.String(), nil
		case "192.168.2.10":
			return "europe", false, nil
		case "192.168.3.10":
			return "america", false, nil
		case "192.168.4.10":
			if primary.Hostname() == ip.String() {
				return "asia", true, nil
			}
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
	if loaded := strings.TrimSpace(string(key)); transportSecret != loaded {
		transportSecret = loaded
	}
	if transportSecret == "" {
		return nil, nil, fmt.Errorf("empty transport key")
	}
	log.Printf("region=%s state_owner=%v", region, owner)
	if region == "compute" {
		a := &App{appCore: &appCore{}}
		return computeHandler(a), a, nil
	}
	if owner {
		a, e := newApp(envOr("INITIAL_DIR", "/home/isucon/isureal-initial"), envOr("STATE_DIR", "/home/isucon/isureal-state"))
		if e != nil {
			return nil, nil, e
		}
		if os.Getenv("REGIONAL_READS") == "1" {
			a.replication, e = newReplicationHub(a, strings.Fields(envOr("READ_REPLICA_REGIONS", "europe america")))
			if e != nil {
				return nil, nil, e
			}
		}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a.replicationHTTP(w, r) || a.ownedOriginalHTTP(w, r) {
				return
			}
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
	if os.Getenv("REGIONAL_READS") == "1" {
		edge, e = newApp(envOr("INITIAL_DIR", "/home/isucon/isureal-initial"), envOr("REPLICA_STATE_DIR", "/home/isucon/isureal-replica-state"))
		if e != nil {
			return nil, nil, e
		}
		edge.localCryptoSlots = make(chan struct{}, 2)
	}
	var uploads *regionalUploads
	if root := os.Getenv("REGIONAL_STATE_DIR"); root != "" {
		uploads, e = newRegionalUploads(root)
		if e != nil {
			return nil, nil, e
		}
		if os.Getenv("REGIONAL_READS") == "1" {
			edge.localUploads = uploads
		} else {
			edge.durable = filepath.Join(root, "pending")
		}
	}
	if os.Getenv("REGIONAL_READS") == "1" {
		edge.replica, e = startReplica(edge, target.String(), region)
		if e != nil {
			return nil, nil, e
		}
	}
	proxy.Transport = measurement.NewUpstreamTransport()
	imageRoot := os.Getenv("IMAGE_CACHE_DIR")
	base := proxy.Director
	proxy.Director = func(r *http.Request) {
		base(r)
		r.Host = target.Host
		r.Header.Set("X-Country-Code", "JP")
		r.Header.Set("X-ISU-Region", region)
		r.Header.Set("X-ISU-Source", region)
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/internal/replication/") {
			failure(w, 403, "forbidden")
			return
		}
		if r.URL.Path == "/healthz" {
			if edge.replica != nil && !edge.replica.ready.Load() {
				failure(w, 503, "replica_starting")
				return
			}
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
		r.Header.Del(regionalImageRequest)
		if edge.replica != nil && edge.replica.ready.Load() && r.Method == "GET" {
			r.Header.Set("X-ISU-Region", region)
			edge.ServeHTTP(w, r)
			return
		}
		if profile, multipart := isImageUpload(r); uploads != nil && (profile || multipart) {
			var p *preparedUpload
			r, p = uploads.prepare(edge, w, r)
			defer uploads.discard(p)
		}
		proxy.ServeHTTP(w, r)
	}), edge, nil
}
