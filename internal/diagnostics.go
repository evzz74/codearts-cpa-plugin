package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/HITZY2002/codearts2api/upstream"
)

type failureDiagnostic struct {
	Category     string `json:"category"`
	Message      string `json:"message"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	UpstreamCode string `json:"upstream_code,omitempty"`
}

var failureMessages = map[string]string{
	"auth_invalid":           "Stored account credentials are invalid",
	"auth_unusable":          "Required account credentials are missing",
	"auth_disabled":          "The account is disabled",
	"auth_not_found":         "The selected account could not be found",
	"auth_store_unavailable": "The selected account could not be read",
	"model_disabled":         "The model is disabled for this account",
	"invalid_request":        "The request was rejected as invalid",
	"upstream_auth":          "Upstream authentication was rejected",
	"upstream_forbidden":     "Upstream denied the request; check account or model entitlement",
	"model_unavailable":      "The upstream model, resource or benefit entitlement is unavailable",
	"rate_limited":           "Upstream rate or quota limit was reached",
	"upstream_unavailable":   "Upstream service is temporarily unavailable",
	"timeout":                "The request timed out",
	"canceled":               "The request was canceled",
	"network":                "The upstream network connection failed",
	"unknown":                "Request failed without a recognized error category",
}

// Only recognized codes may cross the keyless usage/model resource boundary.
func safeUpstreamCode(code string) string {
	switch strings.ToLower(code) {
	case "inferhub.002002009.404":
		return "InferHub.002002009.404"
	case "inferhub.4004.200":
		return "InferHub.4004.200"
	case "inferhub.001001005.400":
		return "InferHub.001001005.400"
	case "inferhub.modelarts.81111.429":
		return "InferHub.ModelArts.81111.429"
	case "tm.00001041":
		return "TM.00001041"
	case "quota.429":
		return "quota.429"
	case "400", "401", "403", "404", "408", "429", "500", "502", "503", "504":
		return code
	default:
		return ""
	}
}

func failureDetails(err error) *failureDiagnostic {
	if err == nil {
		return nil
	}
	d := &failureDiagnostic{Category: "unknown"}
	defer func() { d.Message = failureMessages[d.Category] }()
	var plugin *pluginError
	if errors.As(err, &plugin) {
		if plugin.diagnostic != nil {
			copy := *plugin.diagnostic
			return &copy
		}
		if _, ok := failureMessages[plugin.code]; ok {
			d.Category = plugin.code
		}
		return d
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		d.Category = "timeout"
		return d
	case errors.Is(err, context.Canceled):
		d.Category = "canceled"
		return d
	}
	var api *upstream.ApiError
	var stream *upstream.UpstreamError
	if errors.As(err, &api) {
		d.UpstreamCode = safeUpstreamCode(api.BusinessCode)
		if api.HTTPStatus >= 100 && api.HTTPStatus <= 599 {
			d.HTTPStatus = api.HTTPStatus
		}
	} else if errors.As(err, &stream) {
		d.UpstreamCode = safeUpstreamCode(stream.Code)
		if stream.HTTPStatus >= 100 && stream.HTTPStatus <= 599 {
			d.HTTPStatus = stream.HTTPStatus
		}
	} else {
		var network net.Error
		if errors.As(err, &network) {
			if network.Timeout() {
				d.Category = "timeout"
			} else {
				d.Category = "network"
			}
		}
		return d
	}
	switch d.UpstreamCode {
	case "InferHub.002002009.404", "InferHub.4004.200":
		d.Category = "model_unavailable"
		return d
	case "InferHub.001001005.400":
		d.Category = "invalid_request"
		return d
	case "InferHub.ModelArts.81111.429", "TM.00001041", "quota.429":
		d.Category = "rate_limited"
		return d
	}
	status := d.HTTPStatus
	// HTTP 200 can carry a separate, recognized business failure.
	if status == http.StatusOK && d.UpstreamCode != "" {
		switch d.UpstreamCode {
		case "400":
			status = 400
		case "401":
			status = 401
		case "403":
			status = 403
		case "404":
			status = 404
		case "408":
			status = 408
		case "429":
			status = 429
		case "500", "502", "503", "504":
			status = 502
		}
	}
	switch status {
	case 400, 422:
		d.Category = "invalid_request"
	case 401:
		d.Category = "upstream_auth"
	case 403:
		d.Category = "upstream_forbidden"
	case 404:
		d.Category = "model_unavailable"
	case 408, 504:
		d.Category = "timeout"
	case 429:
		d.Category = "rate_limited"
	default:
		if status >= 500 {
			d.Category = "upstream_unavailable"
		}
	}
	return d
}

func (d *failureDiagnostic) summary() string {
	result := d.Message
	if d.HTTPStatus != 0 {
		result += fmt.Sprintf(" (HTTP %d)", d.HTTPStatus)
	}
	if d.UpstreamCode != "" {
		result += " [" + d.UpstreamCode + "]"
	}
	return result
}

func safeExecutionError(err error) error {
	if err == nil {
		return nil
	}
	d := failureDetails(err)
	status := http.StatusBadGateway
	code := d.Category
	var plugin *pluginError
	var api *upstream.ApiError
	if errors.As(err, &plugin) {
		status = plugin.status
	} else if errors.As(err, &api) {
		status = api.Status
	} else if d.Category == "timeout" {
		status = http.StatusGatewayTimeout
	}
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	return &pluginError{code: code, message: d.summary(), status: status, diagnostic: d}
}
