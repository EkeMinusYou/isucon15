package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	measurement "github.com/EkeMinusYou/isucon-template/tools/measurement/app"
)

type ingressRegionKey struct{}

const coordinatorHeader = "X-ISU-Coordinator"

func ingressRegion(r *http.Request) string {
	if region, ok := r.Context().Value(ingressRegionKey{}).(string); ok && validRegion(region) {
		return region
	}
	return fixedRegion
}

func (m *regionalMaster) route(next http.Handler) (http.Handler, error) {
	proxies := map[string]*httputil.ReverseProxy{}
	for _, region := range masterRegions {
		if region == m.region {
			continue
		}
		target, err := url.Parse(regionalPeer(region))
		if err != nil || target.Host == "" {
			return nil, fmt.Errorf("invalid master address for %s: %q", region, regionalPeer(region))
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.Transport = measurement.NewUpstreamTransport()
		base := proxy.Director
		proxy.Director = func(r *http.Request) {
			base(r)
			r.Host = target.Host
			r.Header.Set("X-ISU-Transport", transportSecret)
			r.Header.Set("X-ISU-Region", ingressRegion(r))
			r.Header.Set("X-ISU-Source", ingressRegion(r))
		}
		proxy.FlushInterval = -1
		proxy.ModifyResponse = func(response *http.Response) error {
			if identity := response.Header.Get("X-ISU-Identity"); identity != "" {
				setMeasurementIdentity(response.Request, identity)
			}
			response.Header.Del("X-ISU-Identity")
			return nil
		}
		proxies[region] = proxy
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internal := isInternal(r)
		entry := m.region
		if internal {
			if !validRegion(r.Header.Get("X-ISU-Region")) {
				failure(w, 400, "invalid_region")
				return
			}
			entry = r.Header.Get("X-ISU-Region")
		} else {
			r.Header.Del("X-ISU-Region")
			r.Header.Del("X-ISU-Source")
			r.Header.Del(regionalUploadHeader)
			r.Header.Del(coordinatorHeader)
		}
		r = r.WithContext(context.WithValue(r.Context(), ingressRegionKey{}, entry))
		if m.internal(w, r) {
			return
		}
		if internal && r.Header.Get(coordinatorHeader) == "1" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/auth/login" {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
			if err != nil {
				failure(w, 413, "body_too_large")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var input struct{ Username string }
			json.NewDecoder(bytes.NewReader(body)).Decode(&input)
			home, err := m.loginHome(r.Context(), input.Username)
			if err != nil {
				failure(w, 503, "home_unavailable")
				return
			}
			if home != "" && home != m.region {
				proxies[home].ServeHTTP(w, r)
			} else {
				m.localHTTP(w, r)
			}
			return
		}
		if ownedLocalRequest(r) {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			home := homeOfNewID(token)
			if home != "" && home != m.region {
				proxies[home].ServeHTTP(w, r)
				return
			}
			if (r.Method == "PATCH" || r.Method == "DELETE") && indexedHome(m.homes.posts, strings.TrimPrefix(r.URL.Path, "/api/posts/")) != m.region {
				r.Header.Set(coordinatorHeader, "1")
				next.ServeHTTP(w, r)
			} else {
				m.localHTTP(w, r)
			}
			return
		}
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/images/") {
			id := strings.TrimPrefix(r.URL.Path, "/api/images/")
			home := indexedHome(m.homes.images, id)
			if home == "" || home == m.region {
				m.getImage(w, r, id)
			} else {
				if m.stats != nil {
					m.stats.forwarded.Add(1)
				}
				proxies[home].ServeHTTP(w, r)
			}
			return
		}
		profile, multipart := isImageUpload(r)
		if (profile || multipart) && !(internal && r.Header.Get(regionalUploadHeader) != "") {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			home := homeOfNewID(token)
			if home != "" && home != m.region {
				proxies[home].ServeHTTP(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	}), nil
}
