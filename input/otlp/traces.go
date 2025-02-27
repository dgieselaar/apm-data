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

// Portions copied from OpenTelemetry Collector (contrib), from the
// elastic exporter.
//
// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otlp

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	semconv "go.opentelemetry.io/collector/semconv/v1.5.0"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/elastic/apm-data/model/modelpb"
)

const (
	keywordLength = 1024
	dot           = "."
	underscore    = "_"

	outcomeSuccess = "success"
	outcomeFailure = "failure"
	outcomeUnknown = "unknown"

	attributeNetworkConnectionType    = "net.host.connection.type"
	attributeNetworkConnectionSubtype = "net.host.connection.subtype"
	attributeNetworkMCC               = "net.host.carrier.mcc"
	attributeNetworkMNC               = "net.host.carrier.mnc"
	attributeNetworkCarrierName       = "net.host.carrier.name"
	attributeNetworkICC               = "net.host.carrier.icc"
)

// ConsumeTraces consumes OpenTelemetry trace data,
// converting into Elastic APM events and reporting to the Elastic APM schema.
func (c *Consumer) ConsumeTraces(ctx context.Context, traces ptrace.Traces) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)

	receiveTimestamp := time.Now()
	c.config.Logger.Debug("consuming traces", zap.Stringer("traces", tracesStringer(traces)))

	resourceSpans := traces.ResourceSpans()
	batch := make(modelpb.Batch, 0, resourceSpans.Len())
	for i := 0; i < resourceSpans.Len(); i++ {
		c.convertResourceSpans(resourceSpans.At(i), receiveTimestamp, &batch)
	}
	return c.config.Processor.ProcessBatch(ctx, &batch)
}

func (c *Consumer) convertResourceSpans(
	resourceSpans ptrace.ResourceSpans,
	receiveTimestamp time.Time,
	out *modelpb.Batch,
) {
	baseEvent := modelpb.APMEvent{
		Event: &modelpb.Event{
			Received: timestamppb.New(receiveTimestamp),
		},
	}
	var timeDelta time.Duration
	resource := resourceSpans.Resource()
	translateResourceMetadata(resource, &baseEvent)
	if exportTimestamp, ok := exportTimestamp(resource); ok {
		timeDelta = receiveTimestamp.Sub(exportTimestamp)
	}
	scopeSpans := resourceSpans.ScopeSpans()
	for i := 0; i < scopeSpans.Len(); i++ {
		c.convertScopeSpans(scopeSpans.At(i), &baseEvent, timeDelta, out)
	}
}

func (c *Consumer) convertScopeSpans(
	in ptrace.ScopeSpans,
	baseEvent *modelpb.APMEvent,
	timeDelta time.Duration,
	out *modelpb.Batch,
) {
	otelSpans := in.Spans()
	for i := 0; i < otelSpans.Len(); i++ {
		c.convertSpan(otelSpans.At(i), in.Scope(), baseEvent, timeDelta, out)
	}
}

func (c *Consumer) convertSpan(
	otelSpan ptrace.Span,
	otelLibrary pcommon.InstrumentationScope,
	baseEvent *modelpb.APMEvent,
	timeDelta time.Duration,
	out *modelpb.Batch,
) {
	root := otelSpan.ParentSpanID().IsEmpty()
	var parentID string
	if !root {
		parentID = hexSpanID(otelSpan.ParentSpanID())
	}

	startTime := otelSpan.StartTimestamp().AsTime()
	endTime := otelSpan.EndTimestamp().AsTime()
	duration := endTime.Sub(startTime)

	// Message consumption results in either a transaction or a span based
	// on whether the consumption is active or passive. Otel spans
	// currently do not have the metadata to make this distinction. For
	// now, we assume that the majority of consumption is passive, and
	// therefore start a transaction whenever span kind == consumer.
	name := otelSpan.Name()
	spanID := hexSpanID(otelSpan.SpanID())
	representativeCount := getRepresentativeCountFromTracestateHeader(otelSpan.TraceState().AsRaw())
	event := baseEvent.CloneVT()
	initEventLabels(event)
	event.Timestamp = timestamppb.New(startTime.Add(timeDelta))
	if id := hexTraceID(otelSpan.TraceID()); id != "" {
		event.Trace = &modelpb.Trace{
			Id: id,
		}
	}
	event.Event = populateNil(event.Event)
	event.Event.Duration = durationpb.New(duration)
	event.Event.Outcome = spanStatusOutcome(otelSpan.Status())
	if parentID != "" {
		event.ParentId = parentID
	}
	if root || otelSpan.Kind() == ptrace.SpanKindServer || otelSpan.Kind() == ptrace.SpanKindConsumer {
		event.Processor = modelpb.TransactionProcessor()
		event.Transaction = &modelpb.Transaction{
			Id:                  spanID,
			Name:                name,
			Sampled:             true,
			RepresentativeCount: representativeCount,
		}
		TranslateTransaction(otelSpan.Attributes(), otelSpan.Status(), otelLibrary, event)
	} else {
		event.Processor = modelpb.SpanProcessor()
		event.Span = &modelpb.Span{
			Id:                  spanID,
			Name:                name,
			RepresentativeCount: representativeCount,
		}
		TranslateSpan(otelSpan.Kind(), otelSpan.Attributes(), event)
	}
	translateSpanLinks(event, otelSpan.Links())
	if len(event.Labels) == 0 {
		event.Labels = nil
	}
	if len(event.NumericLabels) == 0 {
		event.NumericLabels = nil
	}
	*out = append(*out, event)

	events := otelSpan.Events()
	event = event.CloneVT()
	event.Labels = baseEvent.Labels                                  // only copy common labels to span events
	event.NumericLabels = baseEvent.NumericLabels                    // only copy common labels to span events
	event.Event = &modelpb.Event{Received: baseEvent.Event.Received} // only copy event.received to span events
	event.Destination = nil                                          // don't set destination for span events
	for i := 0; i < events.Len(); i++ {
		*out = append(*out, c.convertSpanEvent(events.At(i), event, timeDelta))
	}
}

// TranslateTransaction converts incoming otlp/otel trace data into the
// expected elasticsearch format.
func TranslateTransaction(
	attributes pcommon.Map,
	spanStatus ptrace.Status,
	library pcommon.InstrumentationScope,
	event *modelpb.APMEvent,
) {
	isJaeger := strings.HasPrefix(event.Agent.Name, "Jaeger")

	var (
		netHostName string
		netHostPort int
	)

	var (
		httpScheme     string
		httpURL        string
		httpServerName string
		httpHost       string
		http           modelpb.HTTP
		httpRequest    modelpb.HTTPRequest
		httpResponse   modelpb.HTTPResponse
	)

	var isHTTP, isRPC, isMessaging bool
	var message modelpb.Message

	var samplerType, samplerParam pcommon.Value
	attributes.Range(func(kDots string, v pcommon.Value) bool {
		if isJaeger {
			switch kDots {
			case "sampler.type":
				samplerType = v
				return true
			case "sampler.param":
				samplerParam = v
				return true
			}
		}

		k := replaceDots(kDots)
		switch v.Type() {
		case pcommon.ValueTypeSlice:
			setLabel(k, event, ifaceAttributeValue(v))
		case pcommon.ValueTypeBool:
			setLabel(k, event, ifaceAttributeValue(v))
		case pcommon.ValueTypeDouble:
			setLabel(k, event, ifaceAttributeValue(v))
		case pcommon.ValueTypeInt:
			switch kDots {
			case semconv.AttributeHTTPStatusCode:
				isHTTP = true
				httpResponse.StatusCode = int32(v.Int())
				http.Response = &httpResponse
			case semconv.AttributeNetPeerPort:
				event.Source = populateNil(event.Source)
				event.Source.Port = uint32(v.Int())
			case semconv.AttributeNetHostPort:
				netHostPort = int(v.Int())
			case semconv.AttributeRPCGRPCStatusCode:
				isRPC = true
				event.Transaction.Result = codes.Code(v.Int()).String()
			default:
				setLabel(k, event, ifaceAttributeValue(v))
			}
		case pcommon.ValueTypeMap:
		case pcommon.ValueTypeStr:
			stringval := truncate(v.Str())
			switch kDots {
			// http.*
			case semconv.AttributeHTTPMethod:
				isHTTP = true
				httpRequest.Method = stringval
				http.Request = &httpRequest
			case semconv.AttributeHTTPURL, semconv.AttributeHTTPTarget, "http.path":
				isHTTP = true
				httpURL = stringval
			case semconv.AttributeHTTPHost:
				isHTTP = true
				httpHost = stringval
			case semconv.AttributeHTTPScheme:
				isHTTP = true
				httpScheme = stringval
			case semconv.AttributeHTTPStatusCode:
				if intv, err := strconv.Atoi(stringval); err == nil {
					isHTTP = true
					httpResponse.StatusCode = int32(intv)
					http.Response = &httpResponse
				}
			case "http.protocol":
				if !strings.HasPrefix(stringval, "HTTP/") {
					// Unexpected, store in labels for debugging.
					modelpb.Labels(event.Labels).Set(k, stringval)
					break
				}
				stringval = strings.TrimPrefix(stringval, "HTTP/")
				fallthrough
			case semconv.AttributeHTTPFlavor:
				isHTTP = true
				http.Version = stringval
			case semconv.AttributeHTTPServerName:
				isHTTP = true
				httpServerName = stringval
			case semconv.AttributeHTTPClientIP:
				if ip, err := netip.ParseAddr(stringval); err == nil {
					event.Client = populateNil(event.Client)
					event.Client.Ip = ip.String()
				}
			case semconv.AttributeHTTPUserAgent:
				event.UserAgent = populateNil(event.UserAgent)
				event.UserAgent.Original = stringval

			// net.*
			case semconv.AttributeNetPeerIP:
				event.Source = populateNil(event.Source)
				if ip, err := netip.ParseAddr(stringval); err == nil {
					event.Source.Ip = ip.String()
				}
			case semconv.AttributeNetPeerName:
				event.Source = populateNil(event.Source)
				event.Source.Domain = stringval
			case semconv.AttributeNetHostName:
				netHostName = stringval
			case attributeNetworkConnectionType:
				event.Network = populateNil(event.Network)
				event.Network.Connection = populateNil(event.Network.Connection)
				event.Network.Connection.Type = stringval
			case attributeNetworkConnectionSubtype:
				event.Network = populateNil(event.Network)
				event.Network.Connection = populateNil(event.Network.Connection)
				event.Network.Connection.Subtype = stringval
			case attributeNetworkMCC:
				event.Network = populateNil(event.Network)
				event.Network.Carrier = populateNil(event.Network.Carrier)
				event.Network.Carrier.Mcc = stringval
			case attributeNetworkMNC:
				event.Network = populateNil(event.Network)
				event.Network.Carrier = populateNil(event.Network.Carrier)
				event.Network.Carrier.Mnc = stringval
			case attributeNetworkCarrierName:
				event.Network = populateNil(event.Network)
				event.Network.Carrier = populateNil(event.Network.Carrier)
				event.Network.Carrier.Name = stringval
			case attributeNetworkICC:
				event.Network = populateNil(event.Network)
				event.Network.Carrier = populateNil(event.Network.Carrier)
				event.Network.Carrier.Icc = stringval

			// messaging.*
			case "message_bus.destination", semconv.AttributeMessagingDestination:
				message.QueueName = stringval
				isMessaging = true

			// rpc.*
			//
			// TODO(axw) add RPC fieldset to ECS? Currently we drop these
			// attributes, and rely on the operation name like we do with
			// Elastic APM agents.
			case semconv.AttributeRPCSystem:
				isRPC = true
			case semconv.AttributeRPCGRPCStatusCode:
				isRPC = true
			case semconv.AttributeRPCService:
			case semconv.AttributeRPCMethod:

			// miscellaneous
			case "type":
				event.Transaction.Type = stringval
			case "session.id":
				event.Session = populateNil(event.Session)
				event.Session.Id = stringval
			case semconv.AttributeServiceVersion:
				// NOTE support for sending service.version as a span tag
				// is deprecated, and will be removed in 8.0. Instrumentation
				// should set this as a resource attribute (OTel) or tracer
				// tag (Jaeger).
				event.Service.Version = stringval
			default:
				modelpb.Labels(event.Labels).Set(k, stringval)
			}
		}
		return true
	})

	if event.Transaction.Type == "" {
		switch {
		case isMessaging:
			event.Transaction.Type = "messaging"
		case isHTTP, isRPC:
			event.Transaction.Type = "request"
		default:
			event.Transaction.Type = "unknown"
		}
	}

	if isHTTP {
		if !proto.Equal(&http, &modelpb.HTTP{}) {
			event.Http = &http
		}

		// Set outcome nad result from status code.
		if statusCode := httpResponse.StatusCode; statusCode > 0 {
			if event.Event.Outcome == outcomeUnknown {
				event.Event.Outcome = serverHTTPStatusCodeOutcome(int(statusCode))
			}
			if event.Transaction.Result == "" {
				event.Transaction.Result = httpStatusCodeResult(int(statusCode))
			}
		}

		// Build the modelpb.URL from http{URL,Host,Scheme}.
		httpHost := httpHost
		if httpHost == "" {
			httpHost = httpServerName
			if httpHost == "" {
				httpHost = netHostName
				if httpHost == "" {
					httpHost = event.GetHost().GetHostname()
				}
			}
			if httpHost != "" && netHostPort > 0 {
				httpHost = net.JoinHostPort(httpHost, strconv.Itoa(netHostPort))
			}
		}
		event.Url = modelpb.ParseURL(httpURL, httpHost, httpScheme)
	}
	if isMessaging {
		event.Transaction.Message = &message
	}

	if event.Source != nil {
		if _, err := netip.ParseAddr(event.GetClient().GetIp()); err != nil {
			event.Client = &modelpb.Client{Ip: event.Source.Ip, Port: event.Source.Port, Domain: event.Source.Domain}
		}
	}

	if samplerType != (pcommon.Value{}) {
		// The client has reported its sampling rate, so we can use it to extrapolate span metrics.
		parseSamplerAttributes(samplerType, samplerParam, event)
	}

	if event.Transaction.Result == "" {
		event.Transaction.Result = spanStatusResult(spanStatus)
	}
	if name := library.Name(); name != "" {
		event.Service = populateNil(event.Service)
		event.Service.Framework = &modelpb.Framework{
			Name:    name,
			Version: library.Version(),
		}
	}
}

// TranslateSpan converts incoming otlp/otel trace data into the
// expected elasticsearch format.
func TranslateSpan(spanKind ptrace.SpanKind, attributes pcommon.Map, event *modelpb.APMEvent) {
	isJaeger := strings.HasPrefix(event.GetAgent().GetName(), "Jaeger")

	var (
		netPeerName string
		netPeerIP   string
		netPeerPort int
	)

	var (
		peerService string
		peerAddress string
	)

	var (
		httpURL    string
		httpHost   string
		httpTarget string
		httpScheme = "http"
	)

	var (
		messageSystem          string
		messageOperation       string
		messageTempDestination bool
	)

	var (
		rpcSystem  string
		rpcService string
	)

	var http modelpb.HTTP
	var httpRequest modelpb.HTTPRequest
	var httpResponse modelpb.HTTPResponse
	var message modelpb.Message
	var db modelpb.DB
	var destinationService modelpb.DestinationService
	var serviceTarget modelpb.ServiceTarget
	var isHTTP, isDatabase, isRPC, isMessaging bool
	var samplerType, samplerParam pcommon.Value
	attributes.Range(func(kDots string, v pcommon.Value) bool {
		if isJaeger {
			switch kDots {
			case "sampler.type":
				samplerType = v
				return true
			case "sampler.param":
				samplerParam = v
				return true
			}
		}

		k := replaceDots(kDots)
		switch v.Type() {
		case pcommon.ValueTypeSlice:
			setLabel(k, event, ifaceAttributeValueSlice(v.Slice()))
		case pcommon.ValueTypeBool:
			switch kDots {
			case semconv.AttributeMessagingTempDestination:
				messageTempDestination = v.Bool()
				fallthrough
			default:
				setLabel(k, event, strconv.FormatBool(v.Bool()))
			}
		case pcommon.ValueTypeDouble:
			setLabel(k, event, v.Double())
		case pcommon.ValueTypeInt:
			switch kDots {
			case "http.status_code":
				httpResponse.StatusCode = int32(v.Int())
				http.Response = &httpResponse
				isHTTP = true
			case semconv.AttributeNetPeerPort, "peer.port":
				netPeerPort = int(v.Int())
			case semconv.AttributeRPCGRPCStatusCode:
				rpcSystem = "grpc"
				isRPC = true
			default:
				setLabel(k, event, v.Int())
			}
		case pcommon.ValueTypeStr:
			stringval := truncate(v.Str())

			switch kDots {
			// http.*
			case semconv.AttributeHTTPHost:
				httpHost = stringval
				isHTTP = true
			case semconv.AttributeHTTPScheme:
				httpScheme = stringval
				isHTTP = true
			case semconv.AttributeHTTPTarget:
				httpTarget = stringval
				isHTTP = true
			case semconv.AttributeHTTPURL:
				httpURL = stringval
				isHTTP = true
			case semconv.AttributeHTTPMethod:
				httpRequest.Method = stringval
				http.Request = &httpRequest
				isHTTP = true

			// db.*
			case "sql.query":
				if db.Type == "" {
					db.Type = "sql"
				}
				fallthrough
			case semconv.AttributeDBStatement:
				// Statement should not be truncated, use original string value.
				db.Statement = v.Str()
				isDatabase = true
			case semconv.AttributeDBName, "db.instance":
				db.Instance = stringval
				isDatabase = true
			case semconv.AttributeDBSystem, "db.type":
				db.Type = stringval
				isDatabase = true
			case semconv.AttributeDBUser:
				db.UserName = stringval
				isDatabase = true

			// net.*
			case semconv.AttributeNetPeerName, "peer.hostname":
				netPeerName = stringval
			case semconv.AttributeNetPeerIP, "peer.ipv4", "peer.ipv6":
				netPeerIP = stringval
			case "peer.address":
				peerAddress = stringval
			case attributeNetworkConnectionType:
				event.Network = populateNil(event.Network)
				event.Network.Connection = populateNil(event.Network.Connection)
				event.Network.Connection.Type = stringval
			case attributeNetworkConnectionSubtype:
				event.Network = populateNil(event.Network)
				event.Network.Connection = populateNil(event.Network.Connection)
				event.Network.Connection.Subtype = stringval
			case attributeNetworkMCC:
				event.Network = populateNil(event.Network)
				event.Network.Carrier = populateNil(event.Network.Carrier)
				event.Network.Carrier.Mcc = stringval
			case attributeNetworkMNC:
				event.Network = populateNil(event.Network)
				event.Network.Carrier = populateNil(event.Network.Carrier)
				event.Network.Carrier.Mnc = stringval
			case attributeNetworkCarrierName:
				event.Network = populateNil(event.Network)
				event.Network.Carrier = populateNil(event.Network.Carrier)
				event.Network.Carrier.Name = stringval
			case attributeNetworkICC:
				event.Network = populateNil(event.Network)
				event.Network.Carrier = populateNil(event.Network.Carrier)
				event.Network.Carrier.Icc = stringval

			// session.*
			case "session.id":
				event.Session = populateNil(event.Session)
				event.Session.Id = stringval

			// messaging.*
			case "message_bus.destination", semconv.AttributeMessagingDestination:
				message.QueueName = stringval
				isMessaging = true
			case semconv.AttributeMessagingOperation:
				messageOperation = stringval
				isMessaging = true
			case semconv.AttributeMessagingSystem:
				messageSystem = stringval
				isMessaging = true

			// rpc.*
			//
			// TODO(axw) add RPC fieldset to ECS? Currently we drop these
			// attributes, and rely on the operation name and span type/subtype
			// like we do with Elastic APM agents.
			case semconv.AttributeRPCSystem:
				rpcSystem = stringval
				isRPC = true
			case semconv.AttributeRPCService:
				rpcService = stringval
				isRPC = true
			case semconv.AttributeRPCGRPCStatusCode:
				rpcSystem = "grpc"
				isRPC = true
			case semconv.AttributeRPCMethod:

			// miscellaneous
			case "span.kind": // filter out
			case semconv.AttributePeerService:
				peerService = stringval
			default:
				modelpb.Labels(event.Labels).Set(k, stringval)
			}
		}
		return true
	})

	if netPeerName == "" && (!strings.ContainsRune(peerAddress, ':') || net.ParseIP(peerAddress) != nil) {
		// peer.address is not necessarily a hostname
		// or IP address; it could be something like
		// a JDBC connection string or ip:port. Ignore
		// values containing colons, except for IPv6.
		netPeerName = peerAddress
	}

	destPort := netPeerPort
	destAddr := netPeerName
	if destAddr == "" {
		destAddr = netPeerIP
	}

	var fullURL *url.URL
	if httpURL != "" {
		fullURL, _ = url.Parse(httpURL)
	} else if httpTarget != "" {
		// Build http.url from http.scheme, http.target, etc.
		if u, err := url.Parse(httpTarget); err == nil {
			fullURL = u
			fullURL.Scheme = httpScheme
			if httpHost == "" {
				// Set host from net.peer.*
				httpHost = destAddr
				if destPort > 0 {
					httpHost = net.JoinHostPort(httpHost, strconv.Itoa(destPort))
				}
			}
			fullURL.Host = httpHost
			httpURL = fullURL.String()
		}
	}
	if fullURL != nil {
		var port int
		portString := fullURL.Port()
		if portString != "" {
			port, _ = strconv.Atoi(portString)
		} else {
			port = schemeDefaultPort(fullURL.Scheme)
		}

		// Set destination.{address,port} from the HTTP URL,
		// replacing peer.* based values to ensure consistency.
		destAddr = truncate(fullURL.Hostname())
		if port > 0 {
			destPort = port
		}
	}

	serviceTarget.Name = peerService
	destinationService.Name = peerService
	destinationService.Resource = peerService
	if peerAddress != "" {
		destinationService.Resource = peerAddress
	}

	if isHTTP {
		if httpResponse.StatusCode > 0 && event.Event.Outcome == outcomeUnknown {
			event.Event.Outcome = clientHTTPStatusCodeOutcome(int(httpResponse.StatusCode))
		}
		if !proto.Equal(&http, &modelpb.HTTP{}) {
			event.Http = &http
		}
		event.Url = populateNil(event.Url)
		event.Url.Original = httpURL
	}
	if isDatabase {
		event.Span.Db = &db
	}
	if isMessaging {
		event.Span.Message = &message
	}

	switch {
	case isDatabase:
		event.Span.Type = "db"
		event.Span.Subtype = db.Type
		serviceTarget.Type = event.Span.Type
		if event.Span.Subtype != "" {
			serviceTarget.Type = event.Span.Subtype
			if destinationService.Name == "" {
				// For database requests, we currently just identify
				// the destination service by db.system.
				destinationService.Name = event.Span.Subtype
				destinationService.Resource = event.Span.Subtype
			}
		}
		if db.Instance != "" {
			serviceTarget.Name = db.Instance
		}
	case isMessaging:
		event.Span.Type = "messaging"
		event.Span.Subtype = messageSystem
		if messageOperation == "" && spanKind == ptrace.SpanKindProducer {
			messageOperation = "send"
		}
		event.Span.Action = messageOperation
		serviceTarget.Type = event.Span.Type
		if event.Span.Subtype != "" {
			serviceTarget.Type = event.Span.Subtype
			if destinationService.Name == "" {
				destinationService.Name = event.Span.Subtype
				destinationService.Resource = event.Span.Subtype
			}
		}
		if destinationService.Resource != "" && message.QueueName != "" {
			destinationService.Resource += "/" + message.QueueName
		}
		if message.QueueName != "" && !messageTempDestination {
			serviceTarget.Name = message.QueueName
		}
	case isRPC:
		event.Span.Type = "external"
		event.Span.Subtype = rpcSystem
		serviceTarget.Type = event.Span.Type
		if event.Span.Subtype != "" {
			serviceTarget.Type = event.Span.Subtype
		}
		// Set destination.service.* from the peer address, unless peer.service was specified.
		if destinationService.Name == "" {
			destHostPort := net.JoinHostPort(destAddr, strconv.Itoa(destPort))
			destinationService.Name = destHostPort
			destinationService.Resource = destHostPort
		}
		if rpcService != "" {
			serviceTarget.Name = rpcService
		}
	case isHTTP:
		event.Span.Type = "external"
		subtype := "http"
		event.Span.Subtype = subtype
		serviceTarget.Type = event.Span.Subtype
		if fullURL != nil {
			url := url.URL{Scheme: fullURL.Scheme, Host: fullURL.Host}
			resource := url.Host
			if destPort == schemeDefaultPort(url.Scheme) {
				if fullURL.Port() != "" {
					// Remove the default port from destination.service.name
					url.Host = destAddr
				} else {
					// Add the default port to destination.service.resource
					resource = fmt.Sprintf("%s:%d", resource, destPort)
				}
			}

			serviceTarget.Name = resource
			if destinationService.Name == "" {
				destinationService.Name = url.String()
				destinationService.Resource = resource
			}
		}
	default:
		// Only set event.Span.Type if not already set
		if event.Span.Type == "" {
			switch spanKind {
			case ptrace.SpanKindInternal:
				event.Span.Type = "app"
				event.Span.Subtype = "internal"
			default:
				event.Span.Type = "unknown"
			}
		}
	}

	if destAddr != "" {
		event.Destination = &modelpb.Destination{Address: destAddr, Port: uint32(destPort)}
	}
	if !proto.Equal(&destinationService, &modelpb.DestinationService{}) {
		if destinationService.Type == "" {
			// Copy span type to destination.service.type.
			destinationService.Type = event.Span.Type
		}
		event.Span.DestinationService = &destinationService
	}

	if !proto.Equal(&serviceTarget, &modelpb.ServiceTarget{}) {
		event.Service.Target = &serviceTarget
	}

	if samplerType != (pcommon.Value{}) {
		// The client has reported its sampling rate, so we can use it to extrapolate transaction metrics.
		parseSamplerAttributes(samplerType, samplerParam, event)
	}
}

func parseSamplerAttributes(samplerType, samplerParam pcommon.Value, event *modelpb.APMEvent) {
	switch samplerType := samplerType.Str(); samplerType {
	case "probabilistic":
		probability := samplerParam.Double()
		if probability > 0 && probability <= 1 {
			if event.Span != nil {
				event.Span.RepresentativeCount = 1 / probability
			}
			if event.Transaction != nil {
				event.Transaction.RepresentativeCount = 1 / probability
			}
		}
	default:
		if event.Span != nil {
			event.Span.RepresentativeCount = 0
		}
		if event.Transaction != nil {
			event.Transaction.RepresentativeCount = 0
		}
		modelpb.Labels(event.Labels).Set("sampler_type", samplerType)
		switch samplerParam.Type() {
		case pcommon.ValueTypeBool:
			modelpb.Labels(event.Labels).Set("sampler_param", strconv.FormatBool(samplerParam.Bool()))
		case pcommon.ValueTypeDouble:
			modelpb.NumericLabels(event.NumericLabels).Set("sampler_param", samplerParam.Double())
		}
	}
}

func (c *Consumer) convertSpanEvent(
	spanEvent ptrace.SpanEvent,
	parent *modelpb.APMEvent, // either span or transaction
	timeDelta time.Duration,
) *modelpb.APMEvent {
	event := parent.CloneVT()
	initEventLabels(event)
	event.Transaction = nil // populate fields as required from parent
	event.Span = nil        // populate fields as required from parent
	event.Timestamp = timestamppb.New(spanEvent.Timestamp().AsTime().Add(timeDelta))

	isJaeger := strings.HasPrefix(parent.Agent.Name, "Jaeger")
	if isJaeger {
		event.Error = c.convertJaegerErrorSpanEvent(spanEvent, event)
	} else if spanEvent.Name() == "exception" {
		// Translate exception span events to errors.
		//
		// If it's not Jaeger, we assume OpenTelemetry semantic semconv.
		// Per OpenTelemetry semantic conventions:
		//   `The name of the event MUST be "exception"`
		var exceptionEscaped bool
		var exceptionMessage, exceptionStacktrace, exceptionType string
		spanEvent.Attributes().Range(func(k string, v pcommon.Value) bool {
			switch k {
			case semconv.AttributeExceptionMessage:
				exceptionMessage = v.Str()
			case semconv.AttributeExceptionStacktrace:
				exceptionStacktrace = v.Str()
			case semconv.AttributeExceptionType:
				exceptionType = v.Str()
			case "exception.escaped":
				exceptionEscaped = v.Bool()
			default:
				setLabel(replaceDots(k), event, ifaceAttributeValue(v))
			}
			return true
		})
		if exceptionMessage != "" || exceptionType != "" {
			// Per OpenTelemetry semantic conventions:
			//   `At least one of the following sets of attributes is required:
			//   - exception.type
			//   - exception.message`
			event.Error = convertOpenTelemetryExceptionSpanEvent(
				exceptionType, exceptionMessage, exceptionStacktrace,
				exceptionEscaped, parent.Service.Language.Name,
			)
		}
	}

	if event.Error != nil {
		event.Processor = modelpb.ErrorProcessor()
		setErrorContext(event, parent)
	} else {
		event.Processor = modelpb.LogProcessor()
		event.Message = spanEvent.Name()
		setLogContext(event, parent)
		spanEvent.Attributes().Range(func(k string, v pcommon.Value) bool {
			k = replaceDots(k)
			if isJaeger && k == "message" {
				event.Message = truncate(v.Str())
				return true
			}
			setLabel(k, event, ifaceAttributeValue(v))
			return true
		})
	}
	return event
}

func (c *Consumer) convertJaegerErrorSpanEvent(event ptrace.SpanEvent, apmEvent *modelpb.APMEvent) *modelpb.Error {
	var isError bool
	var exMessage, exType string
	var logMessage string

	if name := truncate(event.Name()); name == "error" {
		isError = true // according to opentracing spec
	} else {
		// Jaeger seems to send the message in the 'event' field.
		//
		// In case 'message' is sent we will use that, otherwise
		// we will use 'event'.
		logMessage = name
	}

	event.Attributes().Range(func(k string, v pcommon.Value) bool {
		if v.Type() != pcommon.ValueTypeStr {
			return true
		}
		stringval := truncate(v.Str())
		switch k {
		case "error", "error.object":
			exMessage = stringval
			isError = true
		case "error.kind":
			exType = stringval
			isError = true
		case "level":
			isError = stringval == "error"
		case "message":
			logMessage = stringval
		default:
			setLabel(replaceDots(k), apmEvent, ifaceAttributeValue(v))
		}
		return true
	})
	if !isError {
		return nil
	}
	if logMessage == "" && exMessage == "" && exType == "" {
		c.config.Logger.Debug(
			"cannot convert span event into Elastic APM error",
			zap.String("name", event.Name()),
		)
		return nil
	}
	e := &modelpb.Error{}
	if logMessage != "" {
		e.Log = &modelpb.ErrorLog{Message: logMessage}
	}
	if exMessage != "" || exType != "" {
		e.Exception = &modelpb.Exception{
			Message: exMessage,
			Type:    exType,
		}
	}
	return e
}

func setErrorContext(out *modelpb.APMEvent, parent *modelpb.APMEvent) {
	out.Trace.Id = parent.Trace.Id
	out.Http = parent.Http
	out.Url = parent.Url
	if parent.Transaction != nil {
		out.Transaction = &modelpb.Transaction{
			Id:      parent.Transaction.Id,
			Sampled: parent.Transaction.Sampled,
			Type:    parent.Transaction.Type,
		}
		out.Error.Custom = parent.Transaction.Custom
		out.ParentId = parent.Transaction.Id
	}
	if parent.Span != nil {
		out.ParentId = parent.Span.Id
	}
}

func setLogContext(out *modelpb.APMEvent, parent *modelpb.APMEvent) {
	if parent.Transaction != nil {
		out.Transaction = &modelpb.Transaction{
			Id: parent.Transaction.Id,
		}
	}
	if parent.Span != nil {
		out.Span = &modelpb.Span{
			Id: parent.Span.Id,
		}
	}
}

func translateSpanLinks(out *modelpb.APMEvent, in ptrace.SpanLinkSlice) {
	n := in.Len()
	if n == 0 {
		return
	}
	out.Span = populateNil(out.Span)
	out.Span.Links = make([]*modelpb.SpanLink, n)
	for i := 0; i < n; i++ {
		link := in.At(i)
		out.Span.Links[i] = &modelpb.SpanLink{
			SpanId:  hexSpanID(link.SpanID()),
			TraceId: hexTraceID(link.TraceID()),
		}
	}
}

func hexSpanID(id pcommon.SpanID) string {
	if id.IsEmpty() {
		return ""
	}
	return hex.EncodeToString(id[:])
}

func hexTraceID(id pcommon.TraceID) string {
	if id.IsEmpty() {
		return ""
	}
	return hex.EncodeToString(id[:])
}

func replaceDots(s string) string {
	return strings.ReplaceAll(s, dot, underscore)
}

// spanStatusOutcome returns the outcome for transactions and spans based on
// the given OTLP span status.
func spanStatusOutcome(status ptrace.Status) string {
	switch status.Code() {
	case ptrace.StatusCodeOk:
		return outcomeSuccess
	case ptrace.StatusCodeError:
		return outcomeFailure
	}
	return outcomeUnknown
}

// spanStatusResult returns the result for transactions based on the given
// OTLP span status. If the span status is unknown, an empty result string
// is returned.
func spanStatusResult(status ptrace.Status) string {
	switch status.Code() {
	case ptrace.StatusCodeOk:
		return "Success"
	case ptrace.StatusCodeError:
		return "Error"
	}
	return ""
}

var standardStatusCodeResults = [...]string{
	"HTTP 1xx",
	"HTTP 2xx",
	"HTTP 3xx",
	"HTTP 4xx",
	"HTTP 5xx",
}

// httpStatusCodeResult returns the transaction result value to use for the
// given HTTP status code.
func httpStatusCodeResult(statusCode int) string {
	switch i := statusCode / 100; i {
	case 1, 2, 3, 4, 5:
		return standardStatusCodeResults[i-1]
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

// serverHTTPStatusCodeOutcome returns the transaction outcome value to use for
// the given HTTP status code.
func serverHTTPStatusCodeOutcome(statusCode int) string {
	if statusCode >= 500 {
		return outcomeFailure
	}
	return outcomeSuccess
}

// clientHTTPStatusCodeOutcome returns the span outcome value to use for the
// given HTTP status code.
func clientHTTPStatusCodeOutcome(statusCode int) string {
	if statusCode >= 400 {
		return outcomeFailure
	}
	return outcomeSuccess
}

// truncate returns s truncated at n runes, and the number of runes in the resulting string (<= n).
func truncate(s string) string {
	var j int
	for i := range s {
		if j == keywordLength {
			return s[:i]
		}
		j++
	}
	return s
}

func schemeDefaultPort(scheme string) int {
	switch scheme {
	case "http":
		return 80
	case "https":
		return 443
	}
	return 0
}

// parses traceparent header, which is expected to be in the W3C Trace-Context
// and searches for the p-value as specified in
//
//	https://opentelemetry.io/docs/reference/specification/trace/tracestate-probability-sampling/#p-value
//
// to calculate the adjusted count (i.e. representative count)
//
// If the p-value is missing or invalid in the tracestate we assume
// a sampling rate of 100% and a representative count of 1.
func getRepresentativeCountFromTracestateHeader(tracestace string) float64 {
	// Default p-value is 0, leading to a default representative count of 1.
	var p uint64 = 0

	otValue := getValueForKeyInString(tracestace, "ot", ',', '=')

	if otValue != "" {
		pValue := getValueForKeyInString(otValue, "p", ';', ':')

		if pValue != "" {
			p, _ = strconv.ParseUint(pValue, 10, 6)
		}
	}

	if p > 62 {
		return 0.0
	}

	return math.Pow(2, float64(p))
}

func getValueForKeyInString(str string, key string, separator rune, assignChar rune) string {
	for {
		str = strings.TrimSpace(str)
		if str == "" {
			break
		}
		kv := str
		if sepIdx := strings.IndexRune(str, separator); sepIdx != -1 {
			kv = strings.TrimSpace(str[:sepIdx])
			str = str[sepIdx+1:]
		} else {
			str = ""
		}
		equal := strings.IndexRune(kv, assignChar)
		if equal != -1 && kv[:equal] == key {
			return kv[equal+1:]
		}
	}

	return ""
}
