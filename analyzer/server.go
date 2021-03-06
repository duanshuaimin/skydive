/*
 * Copyright (C) 2015 Red Hat, Inc.
 *
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 *
 */

package analyzer

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redhat-cip/skydive/api"
	"github.com/redhat-cip/skydive/config"
	"github.com/redhat-cip/skydive/flow"
	"github.com/redhat-cip/skydive/flow/mappings"
	shttp "github.com/redhat-cip/skydive/http"
	"github.com/redhat-cip/skydive/logging"
	"github.com/redhat-cip/skydive/storage"
	"github.com/redhat-cip/skydive/storage/elasticsearch"
	"github.com/redhat-cip/skydive/storage/etcd"
	"github.com/redhat-cip/skydive/topology/alert"
	"github.com/redhat-cip/skydive/topology/graph"
)

type Server struct {
	HTTPServer          *shttp.Server
	WSServer            *shttp.WSServer
	GraphServer         *graph.GraphServer
	AlertServer         *alert.AlertServer
	FlowMappingPipeline *mappings.FlowMappingPipeline
	Storage             storage.Storage
	FlowTable           *flow.Table
	conn                *net.UDPConn
	EmbeddedEtcd        *etcd.EmbeddedEtcd
	EtcdClient          *etcd.EtcdClient
	running             atomic.Value
	wgServers           sync.WaitGroup
}

func (s *Server) flowExpireUpdate(flows []*flow.Flow) {
	if s.Storage != nil {
		s.Storage.StoreFlows(flows)
		logging.GetLogger().Debugf("%d flows stored", len(flows))
	}
}

func (s *Server) AnalyzeFlows(flows []*flow.Flow) {
	s.FlowTable.Update(flows)
	s.FlowMappingPipeline.Enhance(flows)

	logging.GetLogger().Debugf("%d flows received", len(flows))
}

func (s *Server) handleUDPFlowPacket() {
	s.conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
	data := make([]byte, 4096)

	for s.running.Load() == true {
		n, _, err := s.conn.ReadFromUDP(data)
		if err != nil {
			if err.(net.Error).Timeout() == true {
				s.conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
				continue
			}
			if s.running.Load() == false {
				return
			}
			logging.GetLogger().Errorf("Error while reading: %s", err.Error())
			return
		}

		f, err := flow.FromData(data[0:n])
		if err != nil {
			logging.GetLogger().Errorf("Error while parsing flow: %s", err.Error())
		}

		s.AnalyzeFlows([]*flow.Flow{f})
	}
}

func (s *Server) asyncFlowTableExpireUpdated() {
	for s.running.Load() == true {
		select {
		case now := <-s.FlowTable.GetExpireTicker():
			s.FlowTable.Expire(now)
		case now := <-s.FlowTable.GetUpdatedTicker():
			s.FlowTable.Updated(now)
		case <-time.After(time.Second * 1):
		}
	}
}

func (s *Server) ListenAndServe() {
	s.running.Store(true)

	if s.Storage != nil {
		s.Storage.Start()
	}

	s.AlertServer.AlertManager.Start()

	s.wgServers.Add(4)
	go func() {
		defer s.wgServers.Done()
		s.HTTPServer.ListenAndServe()
	}()

	go func() {
		defer s.wgServers.Done()
		s.WSServer.ListenAndServe()
	}()

	go func() {
		defer s.wgServers.Done()

		host := s.HTTPServer.Addr + ":" + strconv.FormatInt(int64(s.HTTPServer.Port), 10)
		addr, err := net.ResolveUDPAddr("udp", host)
		s.conn, err = net.ListenUDP("udp", addr)
		if err != nil {
			panic(err)
		}
		defer s.conn.Close()

		s.handleUDPFlowPacket()
	}()

	go func() {
		defer s.wgServers.Done()
		s.asyncFlowTableExpireUpdated()
	}()
}

func (s *Server) Stop() {
	s.running.Store(false)
	s.FlowTable.UnregisterAll()
	s.WSServer.Stop()
	s.HTTPServer.Stop()
	if s.EmbeddedEtcd != nil {
		s.EmbeddedEtcd.Stop()
	}
	if s.Storage != nil {
		s.Storage.Stop()
	}
	s.AlertServer.AlertManager.Stop()
	s.EtcdClient.Stop()
	s.wgServers.Wait()
	if tr, ok := http.DefaultTransport.(interface {
		CloseIdleConnections()
	}); ok {
		tr.CloseIdleConnections()
	}
}

func (s *Server) Flush() {
	logging.GetLogger().Critical("Flush() MUST be called for testing purpose only, not in production")
	s.FlowTable.ExpireNow()
}

func (s *Server) SetStorage(storage storage.Storage) {
	s.Storage = storage
}

func (s *Server) SetStorageFromConfig() {
	if t := config.GetConfig().GetString("analyzer.storage"); t != "" {
		switch t {
		case "elasticsearch":
			storage, err := elasticsearch.New()
			if err != nil {
				logging.GetLogger().Fatalf("Can't connect to ElasticSearch server: %v", err)
			}
			s.SetStorage(storage)
		default:
			logging.GetLogger().Fatalf("Storage type unknown: %s", t)
			os.Exit(1)
		}
		logging.GetLogger().Infof("Using %s as storage", t)
	}
}

func NewServerFromConfig() (*Server, error) {
	embedEtcd := config.GetConfig().GetBool("etcd.embedded")

	backend, err := graph.BackendFromConfig()
	if err != nil {
		return nil, err
	}

	g, err := graph.NewGraph(backend)
	if err != nil {
		return nil, err
	}

	httpServer, err := shttp.NewServerFromConfig("analyzer")
	if err != nil {
		return nil, err
	}

	wsServer := shttp.NewWSServerFromConfig(httpServer, "/ws")

	api.RegisterTopologyApi("analyzer", g, httpServer)

	var etcdServer *etcd.EmbeddedEtcd
	if embedEtcd {
		if etcdServer, err = etcd.NewEmbeddedEtcdFromConfig(); err != nil {
			return nil, err
		}
	}

	etcdClient, err := etcd.NewEtcdClientFromConfig()
	if err != nil {
		return nil, err
	}

	apiServer, err := api.NewApi(httpServer, etcdClient.KeysApi)
	if err != nil {
		return nil, err
	}

	captureHandler := &api.BasicApiHandler{
		ResourceHandler: &api.CaptureHandler{},
		EtcdKeyAPI:      etcdClient.KeysApi,
	}
	err = apiServer.RegisterApiHandler(captureHandler)
	if err != nil {
		return nil, err
	}

	alertHandler := &api.BasicApiHandler{
		ResourceHandler: &api.AlertHandler{},
		EtcdKeyAPI:      etcdClient.KeysApi,
	}
	err = apiServer.RegisterApiHandler(alertHandler)
	if err != nil {
		return nil, err
	}

	alertManager := alert.NewAlertManager(g, alertHandler)

	aserver := alert.NewServer(alertManager, wsServer)
	gserver := graph.NewServer(g, wsServer)

	gfe := mappings.NewGraphFlowEnhancer(g)
	ofe := mappings.NewOvsFlowEnhancer(g)

	pipeline := mappings.NewFlowMappingPipeline(gfe, ofe)

	flowtable := flow.NewTable()

	server := &Server{
		HTTPServer:          httpServer,
		WSServer:            wsServer,
		GraphServer:         gserver,
		AlertServer:         aserver,
		FlowMappingPipeline: pipeline,
		FlowTable:           flowtable,
		EmbeddedEtcd:        etcdServer,
		EtcdClient:          etcdClient,
	}
	server.SetStorageFromConfig()

	api.RegisterFlowApi("analyzer", flowtable, server.Storage, httpServer)

	analyzerExpire := config.GetAnalyerExpire()
	agentExpire := config.GetAgentExpire()
	flowtable.RegisterExpire(server.flowExpireUpdate, analyzerExpire, agentExpire)

	analyzerUpdate := config.GetAnalyerUpdate()
	agentUpdate := config.GetAgentUpdate()
	flowtable.RegisterUpdated(server.flowExpireUpdate, analyzerUpdate, agentUpdate)

	return server, nil
}
