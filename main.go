// Copyright (c) 2020-2025 Doc.ai and/or its affiliates.
//
// Copyright (c) 2023-2025 Cisco and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !windows
// +build !windows

package main

import (
	"context"
	"crypto/tls"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edwarnicke/grpcfd"

	"github.com/bszirtes/sdk-k8s/pkg/registry/chains/registryk8s"
	"github.com/bszirtes/sdk-k8s/pkg/tools/k8s"

	"github.com/networkservicemesh/sdk/pkg/registry/common/authorize"
	"github.com/networkservicemesh/sdk/pkg/tools/opentelemetry"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"
	"github.com/networkservicemesh/sdk/pkg/tools/token"
	"github.com/networkservicemesh/sdk/pkg/tools/tracing"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/kelseyhightower/envconfig"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/sdk/pkg/tools/debug"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
	"github.com/networkservicemesh/sdk/pkg/tools/pprofutils"
)

// Config is configuration for cmd-registry-memory
type Config struct {
	registryk8s.Config
	ListenOn               []url.URL     `default:"unix:///listen.on.socket" desc:"url to listen on." split_words:"true"`
	MaxTokenLifetime       time.Duration `default:"10m" desc:"maximum lifetime of tokens" split_words:"true"`
	RegistryServerPolicies []string      `default:"etc/nsm/opa/common/.*.rego,etc/nsm/opa/registry/.*.rego,etc/nsm/opa/server/.*.rego" desc:"paths to files and directories that contain registry server policies" split_words:"true"`
	RegistryClientPolicies []string      `default:"etc/nsm/opa/common/.*.rego,etc/nsm/opa/registry/.*.rego,etc/nsm/opa/client/.*.rego" desc:"paths to files and directories that contain registry client policies" split_words:"true"`
	LogLevel               string        `default:"INFO" desc:"Log level" split_words:"true"`
	OpenTelemetryEndpoint  string        `default:"otel-collector.observability.svc.cluster.local:4317" desc:"OpenTelemetry Collector Endpoint" split_words:"true"`
	MetricsExportInterval  time.Duration `default:"10s" desc:"interval between mertics exports" split_words:"true"`
	PprofEnabled           bool          `default:"false" desc:"is pprof enabled" split_words:"true"`
	PprofListenOn          string        `default:"localhost:6060" desc:"pprof URL to ListenAndServe" split_words:"true"`
	// The QPS value is calculated for 40 NSEs, 40 NSCs and 5 FWDs.
	// NSC, FWD and NSE refreshes occur every second
	// NSE Refreshes: 1 refresh per sec. 				* 40 nses
	// FWD Refreshes: 1 refresh per sec. 				* 5 fwds
	// NSC Refreshes: 4 finds (in 1 refresh) per sec. 	* 40 nscs
	// Total:											= 205
	KubeletQPS int `default:"205" desc:"kubelet config settings" split_words:"true"`
}

func main() {
	config := new(Config)
	// Setup context to catch signals
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		// More Linux signals here
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	defer cancel()

	// Setup logging
	log.EnableTracing(true)
	logrus.SetFormatter(&nested.Formatter{})
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[0]}))

	// Debug self if necessary
	if err := debug.Self(); err != nil {
		log.FromContext(ctx).Infof("%s", err)
	}

	startTime := time.Now()

	// Get config from environment
	if err := envconfig.Usage("nsm", config); err != nil {
		logrus.Fatal(err)
	}
	if err := envconfig.Process("nsm", config); err != nil {
		logrus.Fatalf("error processing config from env: %+v", err)
	}

	l, err := logrus.ParseLevel(config.LogLevel)
	if err != nil {
		logrus.Fatalf("invalid log level %s", config.LogLevel)
	}
	logrus.SetLevel(l)
	log.FromContext(ctx).Infof("Config: %#v", config)
	logruslogger.SetupLevelChangeOnSignal(ctx, map[os.Signal]logrus.Level{
		syscall.SIGUSR1: logrus.TraceLevel,
		syscall.SIGUSR2: l,
	})

	// Configure Open Telemetry
	if opentelemetry.IsEnabled() {
		collectorAddress := config.OpenTelemetryEndpoint
		spanExporter := opentelemetry.InitSpanExporter(ctx, collectorAddress)
		metricExporter := opentelemetry.InitOPTLMetricExporter(ctx, collectorAddress, config.MetricsExportInterval)
		o := opentelemetry.Init(ctx, spanExporter, metricExporter, "registry-k8s")
		defer func() {
			if err = o.Close(); err != nil {
				log.FromContext(ctx).Error(err.Error())
			}
		}()
	}

	// Configure pprof
	if config.PprofEnabled {
		go pprofutils.ListenAndServe(ctx, config.PprofListenOn)
	}

	// Get a X509Source
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	svid, err := source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	logrus.Infof("SVID: %q", svid.ID)

	tlsClientConfig := tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny())
	tlsClientConfig.MinVersion = tls.VersionTLS12
	tlsServerConfig := tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeAny())
	tlsServerConfig.MinVersion = tls.VersionTLS12

	credsTLS := credentials.NewTLS(tlsServerConfig)
	// Create GRPC Server and register services
	serverOptions := append(tracing.WithTracing(), grpc.Creds(credsTLS))
	server := grpc.NewServer(serverOptions...)

	clientOptions := append(
		tracing.WithTracingDial(),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime)))),
		grpc.WithTransportCredentials(
			grpcfd.TransportCredentials(credentials.NewTLS(tlsClientConfig))),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
	)

	// Adjust config and create ClientSet
	client, _, err := k8s.NewVersionedClient(
		k8s.WithQPS(float32(config.KubeletQPS)),
		k8s.WithBurst(config.KubeletQPS*2))
	if err != nil {
		logrus.Fatalf("error creating NewVersionedClient: %+v", err)
	}

	config.ClientSet = client
	config.ChainCtx = ctx

	registryk8s.NewServer(
		&config.Config,
		spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime),
		registryk8s.WithAuthorizeNSERegistryServer(authorize.NewNetworkServiceEndpointRegistryServer(authorize.WithPolicies(config.RegistryServerPolicies...))),
		registryk8s.WithAuthorizeNSERegistryClient(authorize.NewNetworkServiceEndpointRegistryClient(authorize.WithPolicies(config.RegistryClientPolicies...))),
		registryk8s.WithAuthorizeNSRegistryServer(authorize.NewNetworkServiceRegistryServer(authorize.WithPolicies(config.RegistryServerPolicies...))),
		registryk8s.WithAuthorizeNSRegistryClient(authorize.NewNetworkServiceRegistryClient(authorize.WithPolicies(config.RegistryClientPolicies...))),
		registryk8s.WithDialOptions(clientOptions...),
	).Register(server)

	for i := 0; i < len(config.ListenOn); i++ {
		srvErrCh := grpcutils.ListenAndServe(ctx, &config.ListenOn[i], server)
		exitOnErr(ctx, cancel, srvErrCh)
	}

	log.FromContext(ctx).Infof("Startup completed in %v", time.Since(startTime))
	<-ctx.Done()
}

func exitOnErr(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		log.FromContext(ctx).Fatal(err)
	default:
	}
	// Otherwise wait for an error in the background to log and cancel
	go func(ctx context.Context, errCh <-chan error) {
		err := <-errCh
		log.FromContext(ctx).Error(err)
		cancel()
	}(ctx, errCh)
}
