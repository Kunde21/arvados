// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"net/http"
	"os"
	"path/filepath"

	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"git.curoverse.com/arvados.git/sdk/go/arvadostest"
	"git.curoverse.com/arvados.git/sdk/go/ctxlog"
	"git.curoverse.com/arvados.git/sdk/go/httpserver"
	check "gopkg.in/check.v1"
)

func integrationTestCluster() *arvados.Cluster {
	cfg, err := arvados.GetConfig(filepath.Join(os.Getenv("WORKSPACE"), "tmp", "arvados.yml"))
	if err != nil {
		panic(err)
	}
	cc, err := cfg.GetCluster("zzzzz")
	if err != nil {
		panic(err)
	}
	return cc
}

// Return a new unstarted controller server, using the Rails API
// provided by the integration-testing environment.
func newServerFromIntegrationTestEnv(c *check.C) *httpserver.Server {
	log := ctxlog.TestLogger(c)

	handler := &Handler{Cluster: &arvados.Cluster{
		ClusterID:  "zzzzz",
		PostgreSQL: integrationTestCluster().PostgreSQL,
		TLS:        arvados.TLS{Insecure: true},
	}}
	arvadostest.SetServiceURL(&handler.Cluster.Services.RailsAPI, "https://"+os.Getenv("ARVADOS_TEST_API_HOST"))
	arvadostest.SetServiceURL(&handler.Cluster.Services.Controller, "http://localhost:/")

	srv := &httpserver.Server{
		Server: http.Server{
			Handler: httpserver.AddRequestIDs(httpserver.LogRequests(log, handler)),
		},
		Addr: ":",
	}
	return srv
}
