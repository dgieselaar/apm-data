// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package modelpb

import (
	"net/http"

	"github.com/elastic/apm-data/model/internal/modeljson"
)

func (h *HTTP) toModelJSON(out *modeljson.HTTP) {
	*out = modeljson.HTTP{
		Version: h.Version,
	}
	if h.Request != nil {
		out.Request = &modeljson.HTTPRequest{
			ID:       h.Request.Id,
			Method:   h.Request.Method,
			Referrer: h.Request.Referrer,
		}
		if h.Request.Headers != nil {
			out.Request.Headers = ToHTTPHeaders(h.Request.Headers)
		}
		if h.Request.Env != nil {
			out.Request.Env = h.Request.Env.AsMap()
		}
		if h.Request.Cookies != nil {
			out.Request.Cookies = h.Request.Cookies.AsMap()
		}
		if h.Request.Body != nil {
			out.Request.Body = &modeljson.HTTPRequestBody{
				Original: h.Request.Body.AsInterface(),
			}
		}
	}
	if h.Response != nil {
		out.Response = &modeljson.HTTPResponse{
			StatusCode:      int(h.Response.StatusCode),
			Finished:        h.Response.Finished,
			HeadersSent:     h.Response.HeadersSent,
			TransferSize:    h.Response.TransferSize,
			EncodedBodySize: h.Response.EncodedBodySize,
			DecodedBodySize: h.Response.DecodedBodySize,
		}
		if h.Response.Headers != nil {
			out.Response.Headers = ToHTTPHeaders(h.Response.Headers)
		}
	}
}

func ToHTTPHeaders(h []*HTTPHeader) http.Header {
	if len(h) == 0 {
		return nil
	}
	headers := make(http.Header, len(h))
	for _, v := range h {
		headers[v.Key] = v.Value
	}
	return headers
}
