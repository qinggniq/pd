// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/gorilla/mux"
	"github.com/pingcap/pd/pkg/apiutil"
	"github.com/pingcap/pd/server/core"
	"github.com/pingcap/pd/server/schedule"
	"github.com/pingcap/pd/server/schedule/filter"
	"github.com/pingcap/pd/server/schedule/operator"
	"github.com/pingcap/pd/server/schedule/opt"
	"github.com/pingcap/pd/server/schedule/selector"
	"github.com/pkg/errors"
	"github.com/unrolled/render"
)

const (
	// EvictLeaderName is evict leader scheduler name.
	EvictLeaderName = "evict-leader-scheduler"
	// EvictLeaderType is evict leader scheduler type.
	EvictLeaderType        = "evict-leader"
	noStoreInSchedulerInfo = "No store in scheduler"
)

func init() {
	schedule.RegisterSliceDecoderBuilder(EvictLeaderType, func(args []string) schedule.ConfigDecoder {
		return func(v interface{}) error {
			if len(args) != 1 {
				return errors.New("should specify the store-id")
			}
			conf, ok := v.(*evictLeaderSchedulerConfig)
			if !ok {
				return ErrScheduleConfigNotExist
			}

			id, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return errors.WithStack(err)
			}
			ranges, err := getKeyRanges(args[1:])
			if err != nil {
				return errors.WithStack(err)
			}
			conf.StoreIDWitRanges[id] = ranges
			return nil

		}
	})

	schedule.RegisterScheduler(EvictLeaderType, func(opController *schedule.OperatorController, storage *core.Storage, decoder schedule.ConfigDecoder) (schedule.Scheduler, error) {
		conf := &evictLeaderSchedulerConfig{StoreIDWitRanges: make(map[uint64][]core.KeyRange), storage: storage}
		if err := decoder(conf); err != nil {
			return nil, err
		}
		conf.cluster = opController.GetCluster()
		return newEvictLeaderScheduler(opController, conf), nil
	})
}

type evictLeaderSchedulerConfig struct {
	mu               sync.RWMutex
	storage          *core.Storage
	StoreIDWitRanges map[uint64][]core.KeyRange `json:"store-id-ranges"`
	cluster          opt.Cluster
}

func (conf *evictLeaderSchedulerConfig) BuildWithArgs(args []string) error {
	if len(args) != 1 {
		return errors.New("should specify the store-id")
	}

	id, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return errors.WithStack(err)
	}
	ranges, err := getKeyRanges(args[1:])
	if err != nil {
		return errors.WithStack(err)
	}
	conf.mu.Lock()
	defer conf.mu.Unlock()
	conf.StoreIDWitRanges[id] = ranges
	return nil
}

func (conf *evictLeaderSchedulerConfig) Clone() *evictLeaderSchedulerConfig {
	conf.mu.RLock()
	defer conf.mu.RUnlock()
	return &evictLeaderSchedulerConfig{
		StoreIDWitRanges: conf.StoreIDWitRanges,
	}
}

func (conf *evictLeaderSchedulerConfig) Persist() error {
	name := conf.getSchedulerName()
	conf.mu.RLock()
	defer conf.mu.RUnlock()
	data, err := schedule.EncodeConfig(conf)
	if err != nil {
		return err
	}
	conf.storage.SaveScheduleConfig(name, data)
	return nil
}

func (conf *evictLeaderSchedulerConfig) getSchedulerName() string {
	return EvictLeaderName
}

func (conf *evictLeaderSchedulerConfig) getRanges(id uint64) []string {
	conf.mu.RLock()
	defer conf.mu.RUnlock()
	var res []string
	for index := range conf.StoreIDWitRanges[id] {
		res = append(res, (string)(conf.StoreIDWitRanges[id][index].StartKey))
		res = append(res, (string)(conf.StoreIDWitRanges[id][index].EndKey))
	}
	return res
}

type evictLeaderScheduler struct {
	*baseScheduler
	conf     *evictLeaderSchedulerConfig
	selector *selector.RandomSelector
	handler  http.Handler
}

// newEvictLeaderScheduler creates an admin scheduler that transfers all leaders
// out of a store.
func newEvictLeaderScheduler(opController *schedule.OperatorController, conf *evictLeaderSchedulerConfig) schedule.Scheduler {
	filters := []filter.Filter{
		filter.StoreStateFilter{ActionScope: EvictLeaderName, TransferLeader: true},
	}

	base := newBaseScheduler(opController)
	handler := newEvictLeaderHandler(conf)
	return &evictLeaderScheduler{
		baseScheduler: base,
		conf:          conf,
		selector:      selector.NewRandomSelector(filters),
		handler:       handler,
	}
}

func (s *evictLeaderScheduler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *evictLeaderScheduler) GetName() string {
	return EvictLeaderName
}

func (s *evictLeaderScheduler) GetType() string {
	return EvictLeaderType
}

func (s *evictLeaderScheduler) EncodeConfig() ([]byte, error) {
	s.conf.mu.RLock()
	defer s.conf.mu.RUnlock()
	return schedule.EncodeConfig(s.conf)
}

func (s *evictLeaderScheduler) Prepare(cluster opt.Cluster) error {
	s.conf.mu.RLock()
	defer s.conf.mu.RUnlock()
	var res error
	for id := range s.conf.StoreIDWitRanges {
		if err := cluster.BlockStore(id); err != nil {
			res = err
		}
	}
	return res
}

func (s *evictLeaderScheduler) Cleanup(cluster opt.Cluster) {
	s.conf.mu.RLock()
	defer s.conf.mu.RUnlock()
	for id := range s.conf.StoreIDWitRanges {
		cluster.UnblockStore(id)
	}
}

func (s *evictLeaderScheduler) IsScheduleAllowed(cluster opt.Cluster) bool {
	return s.opController.OperatorCount(operator.OpLeader) < cluster.GetLeaderScheduleLimit()
}

func (s *evictLeaderScheduler) Schedule(cluster opt.Cluster) []*operator.Operator {
	schedulerCounter.WithLabelValues(s.GetName(), "schedule").Inc()
	var ops []*operator.Operator
	s.conf.mu.RLock()
	defer s.conf.mu.RUnlock()
	for id, ranges := range s.conf.StoreIDWitRanges {
		region := cluster.RandLeaderRegion(id, ranges, opt.HealthRegion(cluster))
		if region == nil {
			schedulerCounter.WithLabelValues(s.GetName(), "no-leader").Inc()
			continue
		}
		target := s.selector.SelectTarget(cluster, cluster.GetFollowerStores(region))
		if target == nil {
			schedulerCounter.WithLabelValues(s.GetName(), "no-target-store").Inc()
			continue
		}
		schedulerCounter.WithLabelValues(s.GetName(), "new-operator").Inc()
		op := operator.CreateTransferLeaderOperator(EvictLeaderType, region, region.GetLeader().GetStoreId(), target.GetID(), operator.OpLeader)
		op.SetPriorityLevel(core.HighPriority)
		ops = append(ops, op)
	}

	return ops
}

type evictLeaderHandler struct {
	rd     *render.Render
	config *evictLeaderSchedulerConfig
}

func (handler *evictLeaderHandler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	var input map[string]interface{}
	if err := apiutil.ReadJSONRespondError(handler.rd, w, r.Body, &input); err != nil {
		return
	}
	var args []string
	var exists bool
	var id uint64
	idFloat, ok := input["store_id"].(float64)
	if ok {
		id = (uint64)(idFloat)
		if _, exists = handler.config.StoreIDWitRanges[id]; !exists {
			if err := handler.config.cluster.BlockStore(id); err != nil {
				handler.rd.JSON(w, http.StatusInternalServerError, err)
				return
			}
		}
		args = append(args, strconv.FormatUint(id, 10))
	}

	ranges, ok := (input["ranges"]).([]string)
	if ok {
		args = append(args, ranges...)
	} else if exists {
		args = append(args, handler.config.getRanges(id)...)
	}

	handler.config.BuildWithArgs(args)
	err := handler.config.Persist()
	if err != nil {
		handler.rd.JSON(w, http.StatusInternalServerError, err)
	}
	handler.rd.JSON(w, http.StatusOK, nil)
}

func (handler *evictLeaderHandler) ListConfig(w http.ResponseWriter, r *http.Request) {
	conf := handler.config.Clone()
	handler.rd.JSON(w, http.StatusOK, conf)
}

func (handler *evictLeaderHandler) DeleteConfig(w http.ResponseWriter, r *http.Request) {
	idStr := mux.Vars(r)["store_id"]
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		handler.rd.JSON(w, http.StatusBadRequest, err.Error())
		return
	}

	handler.config.mu.Lock()
	defer handler.config.mu.Unlock()
	_, exists := handler.config.StoreIDWitRanges[id]
	if exists {
		delete(handler.config.StoreIDWitRanges, id)
		handler.config.cluster.UnblockStore(id)

		handler.config.mu.Unlock()
		handler.config.Persist()
		handler.config.mu.Lock()

		var resp interface{}
		if len(handler.config.StoreIDWitRanges) == 0 {
			resp = noStoreInSchedulerInfo
		}
		handler.rd.JSON(w, http.StatusOK, resp)
		return
	}

	handler.rd.JSON(w, http.StatusInternalServerError, ErrScheduleConfigNotExist)
}

func newEvictLeaderHandler(config *evictLeaderSchedulerConfig) http.Handler {
	h := &evictLeaderHandler{
		config: config,
		rd:     render.New(render.Options{IndentJSON: true}),
	}
	router := mux.NewRouter()
	router.HandleFunc("/config", h.UpdateConfig).Methods("POST")
	router.HandleFunc("/list", h.ListConfig).Methods("GET")
	router.HandleFunc("/delete/{store_id}", h.DeleteConfig).Methods("DELETE")
	return router
}
