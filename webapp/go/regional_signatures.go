package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const regionalSignRequest = "X-ISU-Regional-Signing"
const regionalSignResponse = "X-ISU-Unsigned-Images"
const unsignedImageMarker = "_isu_image_reference"

func regionalSigningRequest(r *http.Request) bool {
	path := r.URL.Path
	return r.Method != http.MethodHead && path != "/api/notifications" && path != "/api/me/export" && !strings.HasPrefix(path, "/api/images/")
}

// Private image references exist only on the authenticated owner-to-edge hop.
// The edge must replace all references before forwarding any response bytes.
func regionalReferences(value any, signer *App) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		if raw, exists := v[unsignedImageMarker]; exists {
			parts, ok := raw.([]any)
			if !ok || len(parts) != 3 || len(v) != 1 {
				return nil, fmt.Errorf("invalid private image reference")
			}
			id, idOK := parts[0].(string)
			secret, keyOK := parts[1].(string)
			number, numberOK := parts[2].(json.Number)
			exp, err := number.Int64()
			if !idOK || !keyOK || !numberOK || err != nil {
				return nil, fmt.Errorf("invalid private image fields")
			}
			return imageReference{signer, id, secret, exp}, nil
		}
		for key, child := range v {
			replaced, err := regionalReferences(child, signer)
			if err != nil {
				return nil, err
			}
			v[key] = replaced
		}
	case []any:
		for i, child := range v {
			replaced, err := regionalReferences(child, signer)
			if err != nil {
				return nil, err
			}
			v[i] = replaced
		}
	}
	return value, nil
}

func signRegionalResponse(resp *http.Response, signer *App) error {
	marked := resp.Header.Get(regionalSignResponse) == "1"
	resp.Header.Del(regionalSignResponse)
	if !marked || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("private signing response is not JSON")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if err != nil || len(body) > 32<<20 {
		return fmt.Errorf("read private signing response: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err = decoder.Decode(&value); err != nil {
		return err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing private signing data")
	}
	value, err = regionalReferences(value, signer)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if resp.Request != nil {
		ctx = resp.Request.Context()
	}
	prefetchImageSignatures(ctx, value)
	body, err = json.Marshal(value)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}
