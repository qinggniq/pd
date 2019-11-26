// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/kvproto/pkg/eraftpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/pd/pkg/mock/mockhbstream"
	"github.com/pingcap/pd/pkg/mock/mockid"
	"github.com/pingcap/pd/pkg/testutil"
	"github.com/pingcap/pd/server/config"
	"github.com/pingcap/pd/server/core"
	"github.com/pingcap/pd/server/id"
	"github.com/pingcap/pd/server/kv"
	syncer "github.com/pingcap/pd/server/region_syncer"
	"github.com/pingcap/pd/server/schedule"
	"github.com/pingcap/pd/server/schedule/operator"
	"github.com/pingcap/pd/server/schedule/opt"
	"github.com/pingcap/pd/server/schedulers"
	"github.com/pingcap/pd/server/statistics"
)

func newTestScheduleConfig() (*config.ScheduleConfig, *config.ScheduleOption, error) {
	cfg := config.NewConfig()
	cfg.Schedule.TolerantSizeRatio = 5
	cfg.Schedule.StoreBalanceRate = 60
	if err := cfg.Adjust(nil); err != nil {
		return nil, nil, err
	}
	opt := config.NewScheduleOption(cfg)
	opt.SetClusterVersion(MinSupportedVersion(Version2_0))
	return &cfg.Schedule, opt, nil
}

func newTestOperator(regionID uint64, regionEpoch *metapb.RegionEpoch, kind operator.OpKind, steps ...operator.OpStep) *operator.Operator {
	return operator.NewOperator("test", "test", regionID, regionEpoch, kind, steps...)
}

type testCluster struct {
	*RaftCluster
}

func newTestCluster(opt *config.ScheduleOption) *testCluster {
	cluster := createTestRaftCluster(mockid.NewIDAllocator(), opt, core.NewStorage(kv.NewMemoryKV()))
	return &testCluster{RaftCluster: cluster}
}

func newTestRegionMeta(regionID uint64) *metapb.Region {
	return &metapb.Region{
		Id:          regionID,
		StartKey:    []byte(fmt.Sprintf("%20d", regionID)),
		EndKey:      []byte(fmt.Sprintf("%20d", regionID+1)),
		RegionEpoch: &metapb.RegionEpoch{Version: 1, ConfVer: 1},
	}
}

func (c *testCluster) addRegionStore(storeID uint64, regionCount int, regionSizes ...uint64) error {
	var regionSize uint64
	if len(regionSizes) == 0 {
		regionSize = uint64(regionCount) * 10
	} else {
		regionSize = regionSizes[0]
	}

	stats := &pdpb.StoreStats{}
	stats.Capacity = 1000 * (1 << 20)
	stats.Available = stats.Capacity - regionSize
	newStore := core.NewStoreInfo(&metapb.Store{Id: storeID},
		core.SetStoreStats(stats),
		core.SetRegionCount(regionCount),
		core.SetRegionSize(int64(regionSize)),
		core.SetLastHeartbeatTS(time.Now()),
	)
	c.Lock()
	defer c.Unlock()
	return c.putStoreLocked(newStore)
}

func (c *testCluster) addLeaderRegion(regionID uint64, leaderStoreID uint64, followerStoreIDs ...uint64) error {
	region := newTestRegionMeta(regionID)
	leader, _ := c.AllocPeer(leaderStoreID)
	region.Peers = []*metapb.Peer{leader}
	for _, followerStoreID := range followerStoreIDs {
		peer, _ := c.AllocPeer(followerStoreID)
		region.Peers = append(region.Peers, peer)
	}
	regionInfo := core.NewRegionInfo(region, leader, core.SetApproximateSize(10), core.SetApproximateKeys(10))
	return c.putRegion(regionInfo)
}

func (c *testCluster) updateLeaderCount(storeID uint64, leaderCount int) error {
	store := c.GetStore(storeID)
	newStore := store.Clone(
		core.SetLeaderCount(leaderCount),
		core.SetLeaderSize(int64(leaderCount)*10),
	)
	c.Lock()
	defer c.Unlock()
	return c.putStoreLocked(newStore)
}

func (c *testCluster) addLeaderStore(storeID uint64, leaderCount int) error {
	stats := &pdpb.StoreStats{}
	newStore := core.NewStoreInfo(&metapb.Store{Id: storeID},
		core.SetStoreStats(stats),
		core.SetLeaderCount(leaderCount),
		core.SetLeaderSize(int64(leaderCount)*10),
		core.SetLastHeartbeatTS(time.Now()),
	)
	c.Lock()
	defer c.Unlock()
	return c.putStoreLocked(newStore)
}

func (c *testCluster) setStoreDown(storeID uint64) error {
	store := c.GetStore(storeID)
	newStore := store.Clone(
		core.SetStoreState(metapb.StoreState_Up),
		core.SetLastHeartbeatTS(time.Time{}),
	)
	c.Lock()
	defer c.Unlock()
	return c.putStoreLocked(newStore)
}

func (c *testCluster) setStoreOffline(storeID uint64) error {
	store := c.GetStore(storeID)
	newStore := store.Clone(core.SetStoreState(metapb.StoreState_Offline))
	c.Lock()
	defer c.Unlock()
	return c.putStoreLocked(newStore)
}

func (c *testCluster) LoadRegion(regionID uint64, followerStoreIDs ...uint64) error {
	//  regions load from etcd will have no leader
	region := newTestRegionMeta(regionID)
	region.Peers = []*metapb.Peer{}
	for _, id := range followerStoreIDs {
		peer, _ := c.AllocPeer(id)
		region.Peers = append(region.Peers, peer)
	}
	return c.putRegion(core.NewRegionInfo(region, nil))
}

var _ = Suite(&testCoordinatorSuite{})

type testCoordinatorSuite struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *testCoordinatorSuite) SetUpSuite(c *C) {
	s.ctx, s.cancel = context.WithCancel(context.Background())
}

func (s *testCoordinatorSuite) TearDownSuite(c *C) {
	s.cancel()
}

func (s *testCoordinatorSuite) TestBasic(c *C) {
	tc, co, cleanup := prepare(nil, nil, nil, c)
	defer cleanup()
	oc := co.opController

	c.Assert(tc.addLeaderRegion(1, 1), IsNil)

	op1 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), operator.OpLeader)
	oc.AddWaitingOperator(op1)
	c.Assert(oc.OperatorCount(op1.Kind()), Equals, uint64(1))
	c.Assert(oc.GetOperator(1).RegionID(), Equals, op1.RegionID())

	// Region 1 already has an operator, cannot add another one.
	op2 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), operator.OpRegion)
	oc.AddWaitingOperator(op2)
	c.Assert(oc.OperatorCount(op2.Kind()), Equals, uint64(0))

	// Remove the operator manually, then we can add a new operator.
	c.Assert(oc.RemoveOperator(op1), IsTrue)
	oc.AddWaitingOperator(op2)
	c.Assert(oc.OperatorCount(op2.Kind()), Equals, uint64(1))
	c.Assert(oc.GetOperator(1).RegionID(), Equals, op2.RegionID())
}

func (s *testCoordinatorSuite) TestDispatch(c *C) {
	tc, co, cleanup := prepare(nil, nil, func(co *coordinator) { co.run() }, c)
	defer cleanup()

	// Transfer peer from store 4 to store 1.
	c.Assert(tc.addRegionStore(4, 40), IsNil)
	c.Assert(tc.addRegionStore(3, 30), IsNil)
	c.Assert(tc.addRegionStore(2, 20), IsNil)
	c.Assert(tc.addRegionStore(1, 10), IsNil)
	c.Assert(tc.addLeaderRegion(1, 2, 3, 4), IsNil)

	// Transfer leader from store 4 to store 2.
	c.Assert(tc.updateLeaderCount(4, 50), IsNil)
	c.Assert(tc.updateLeaderCount(3, 50), IsNil)
	c.Assert(tc.updateLeaderCount(2, 20), IsNil)
	c.Assert(tc.updateLeaderCount(1, 10), IsNil)
	c.Assert(tc.addLeaderRegion(2, 4, 3, 2), IsNil)

	// Wait for schedule and turn off balance.
	waitOperator(c, co, 1)
	testutil.CheckTransferPeer(c, co.opController.GetOperator(1), operator.OpBalance, 4, 1)
	c.Assert(co.removeScheduler(schedulers.BalanceRegionName), IsNil)
	waitOperator(c, co, 2)
	testutil.CheckTransferLeader(c, co.opController.GetOperator(2), operator.OpBalance, 4, 2)
	c.Assert(co.removeScheduler(schedulers.BalanceLeaderName), IsNil)

	stream := mockhbstream.NewHeartbeatStream()

	// Transfer peer.
	region := tc.GetRegion(1).Clone()
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitAddLearner(c, stream, region, 1)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitPromoteLearner(c, stream, region, 1)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitRemovePeer(c, stream, region, 4)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitNoResponse(c, stream)

	// Transfer leader.
	region = tc.GetRegion(2).Clone()
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitTransferLeader(c, stream, region, 2)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitNoResponse(c, stream)
}

func dispatchHeartbeat(co *coordinator, region *core.RegionInfo, stream mockhbstream.HeartbeatStream) error {
	co.hbStreams.bindStream(region.GetLeader().GetStoreId(), stream)
	if err := co.cluster.putRegion(region.Clone()); err != nil {
		return err
	}
	co.opController.Dispatch(region, schedule.DispatchFromHeartBeat)
	return nil
}

func (s *testCoordinatorSuite) TestCollectMetrics(c *C) {
	tc, co, cleanup := prepare(nil, func(tc *testCluster) {
		tc.regionStats = statistics.NewRegionStatistics(tc.s.scheduleOpt)
	}, func(co *coordinator) { co.run() }, c)
	defer cleanup()

	// Make sure there are no problem when concurrent write and read
	var wg sync.WaitGroup
	count := 10
	wg.Add(count + 1)
	for i := 0; i <= count; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Assert(tc.addRegionStore(uint64(i%5), rand.Intn(200)), IsNil)
			}
		}(i)
	}
	for i := 0; i < 1000; i++ {
		co.collectHotSpotMetrics()
		co.collectSchedulerMetrics()
		co.cluster.collectClusterMetrics()
	}
	co.resetHotSpotMetrics()
	co.resetSchedulerMetrics()
	co.cluster.resetClusterMetrics()
	wg.Wait()
}

func MaxUint64(nums ...uint64) uint64 {
	result := uint64(0)
	for _, num := range nums {
		if num > result {
			result = num
		}
	}
	return result
}

func prepare(setCfg func(*config.ScheduleConfig), setTc func(*testCluster), run func(*coordinator), c *C) (*testCluster, *coordinator, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg, opt, err := newTestScheduleConfig()
	c.Assert(err, IsNil)
	if setCfg != nil {
		setCfg(cfg)
	}
	tc := newTestCluster(opt)
	hbStreams, cleanup := getHeartBeatStreams(ctx, c, tc)
	if setTc != nil {
		setTc(tc)
	}
	co := newCoordinator(ctx, tc.RaftCluster, hbStreams)
	if run != nil {
		run(co)
	}
	return tc, co, func() {
		co.stop()
		co.wg.Wait()
		cleanup()
		hbStreams.Close()
		cancel()
	}
}

func (s *testCoordinatorSuite) checkRegion(c *C, tc *testCluster, co *coordinator, regionID uint64, expectCheckerIsBusy, expectAddOperator bool) {
	checkerIsBusy, ops := co.checkers.CheckRegion(tc.GetRegion(regionID))
	c.Assert(checkerIsBusy, Equals, expectCheckerIsBusy)
	if ops == nil {
		c.Assert(expectAddOperator, IsFalse)
	} else {
		c.Assert(co.opController.AddWaitingOperator(ops...), Equals, expectAddOperator)
	}
}

func (s *testCoordinatorSuite) TestCheckRegion(c *C) {
	tc, co, cleanup := prepare(nil, nil, func(co *coordinator) { co.run() }, c)
	hbStreams, opt := co.hbStreams, tc.opt
	defer cleanup()

	c.Assert(tc.addRegionStore(4, 4), IsNil)
	c.Assert(tc.addRegionStore(3, 3), IsNil)
	c.Assert(tc.addRegionStore(2, 2), IsNil)
	c.Assert(tc.addRegionStore(1, 1), IsNil)
	c.Assert(tc.addLeaderRegion(1, 2, 3), IsNil)
	s.checkRegion(c, tc, co, 1, false, true)
	waitOperator(c, co, 1)
	testutil.CheckAddPeer(c, co.opController.GetOperator(1), operator.OpReplica, 1)
	s.checkRegion(c, tc, co, 1, false, false)

	r := tc.GetRegion(1)
	p := &metapb.Peer{Id: 1, StoreId: 1, IsLearner: true}
	r = r.Clone(
		core.WithAddPeer(p),
		core.WithPendingPeers(append(r.GetPendingPeers(), p)),
	)
	c.Assert(tc.putRegion(r), IsNil)
	s.checkRegion(c, tc, co, 1, false, false)
	co.stop()
	co.wg.Wait()

	tc = newTestCluster(opt)
	co = newCoordinator(s.ctx, tc.RaftCluster, hbStreams)
	co.run()

	c.Assert(tc.addRegionStore(4, 4), IsNil)
	c.Assert(tc.addRegionStore(3, 3), IsNil)
	c.Assert(tc.addRegionStore(2, 2), IsNil)
	c.Assert(tc.addRegionStore(1, 1), IsNil)
	c.Assert(tc.putRegion(r), IsNil)
	s.checkRegion(c, tc, co, 1, false, false)
	r = r.Clone(core.WithPendingPeers(nil))
	c.Assert(tc.putRegion(r), IsNil)
	s.checkRegion(c, tc, co, 1, false, true)
	waitOperator(c, co, 1)
	op := co.opController.GetOperator(1)
	c.Assert(op.Len(), Equals, 1)
	c.Assert(op.Step(0).(operator.PromoteLearner).ToStore, Equals, uint64(1))
	s.checkRegion(c, tc, co, 1, false, false)
}

func (s *testCoordinatorSuite) TestCheckerIsBusy(c *C) {
	tc, co, cleanup := prepare(func(cfg *config.ScheduleConfig) {
		cfg.ReplicaScheduleLimit = 0 // ensure replica checker is busy
		cfg.MergeScheduleLimit = 10
	}, nil, func(co *coordinator) { co.run() }, c)
	defer cleanup()

	c.Assert(tc.addRegionStore(1, 0), IsNil)
	num := 1 + MaxUint64(co.cluster.GetReplicaScheduleLimit(), co.cluster.GetMergeScheduleLimit())
	var operatorKinds = []operator.OpKind{
		operator.OpReplica, operator.OpRegion | operator.OpMerge,
	}
	for i, operatorKind := range operatorKinds {
		for j := uint64(0); j < num; j++ {
			regionID := j + uint64(i+1)*num
			c.Assert(tc.addLeaderRegion(regionID, 1), IsNil)
			switch operatorKind {
			case operator.OpReplica:
				op := newTestOperator(regionID, tc.GetRegion(regionID).GetRegionEpoch(), operatorKind)
				c.Assert(co.opController.AddWaitingOperator(op), IsTrue)
			case operator.OpRegion | operator.OpMerge:
				if regionID%2 == 1 {
					ops, err := operator.CreateMergeRegionOperator("merge-region", co.cluster, tc.GetRegion(regionID), tc.GetRegion(regionID-1), operator.OpMerge)
					c.Assert(err, IsNil)
					c.Assert(co.opController.AddWaitingOperator(ops...), IsTrue)
				}
			}

		}
	}
	s.checkRegion(c, tc, co, num, true, false)
}

func (s *testCoordinatorSuite) TestReplica(c *C) {
	tc, co, cleanup := prepare(func(cfg *config.ScheduleConfig) {
		// Turn off balance.
		cfg.LeaderScheduleLimit = 0
		cfg.RegionScheduleLimit = 0
	}, nil, func(co *coordinator) { co.run() }, c)
	defer cleanup()

	c.Assert(tc.addRegionStore(1, 1), IsNil)
	c.Assert(tc.addRegionStore(2, 2), IsNil)
	c.Assert(tc.addRegionStore(3, 3), IsNil)
	c.Assert(tc.addRegionStore(4, 4), IsNil)

	stream := mockhbstream.NewHeartbeatStream()

	// Add peer to store 1.
	c.Assert(tc.addLeaderRegion(1, 2, 3), IsNil)
	region := tc.GetRegion(1)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitAddLearner(c, stream, region, 1)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitPromoteLearner(c, stream, region, 1)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitNoResponse(c, stream)

	// Peer in store 3 is down, remove peer in store 3 and add peer to store 4.
	c.Assert(tc.setStoreDown(3), IsNil)
	downPeer := &pdpb.PeerStats{
		Peer:        region.GetStorePeer(3),
		DownSeconds: 24 * 60 * 60,
	}
	region = region.Clone(
		core.WithDownPeers(append(region.GetDownPeers(), downPeer)),
	)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitAddLearner(c, stream, region, 4)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitPromoteLearner(c, stream, region, 4)
	region = region.Clone(core.WithDownPeers(nil))
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitNoResponse(c, stream)

	// Remove peer from store 4.
	c.Assert(tc.addLeaderRegion(2, 1, 2, 3, 4), IsNil)
	region = tc.GetRegion(2)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitRemovePeer(c, stream, region, 4)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitNoResponse(c, stream)

	// Remove offline peer directly when it's pending.
	c.Assert(tc.addLeaderRegion(3, 1, 2, 3), IsNil)
	c.Assert(tc.setStoreOffline(3), IsNil)
	region = tc.GetRegion(3)
	region = region.Clone(core.WithPendingPeers([]*metapb.Peer{region.GetStorePeer(3)}))
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitNoResponse(c, stream)
}

func (s *testCoordinatorSuite) TestPeerState(c *C) {
	tc, co, cleanup := prepare(nil, nil, func(co *coordinator) { co.run() }, c)
	defer cleanup()

	// Transfer peer from store 4 to store 1.
	c.Assert(tc.addRegionStore(1, 10), IsNil)
	c.Assert(tc.addRegionStore(2, 10), IsNil)
	c.Assert(tc.addRegionStore(3, 10), IsNil)
	c.Assert(tc.addRegionStore(4, 40), IsNil)
	c.Assert(tc.addLeaderRegion(1, 2, 3, 4), IsNil)

	stream := mockhbstream.NewHeartbeatStream()

	// Wait for schedule.
	waitOperator(c, co, 1)
	testutil.CheckTransferPeer(c, co.opController.GetOperator(1), operator.OpBalance, 4, 1)

	region := tc.GetRegion(1).Clone()

	// Add new peer.
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitAddLearner(c, stream, region, 1)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitPromoteLearner(c, stream, region, 1)

	// If the new peer is pending, the operator will not finish.
	region = region.Clone(core.WithPendingPeers(append(region.GetPendingPeers(), region.GetStorePeer(1))))
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitNoResponse(c, stream)
	c.Assert(co.opController.GetOperator(region.GetID()), NotNil)

	// The new peer is not pending now, the operator will finish.
	// And we will proceed to remove peer in store 4.
	region = region.Clone(core.WithPendingPeers(nil))
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitRemovePeer(c, stream, region, 4)
	c.Assert(tc.addLeaderRegion(1, 1, 2, 3), IsNil)
	region = tc.GetRegion(1).Clone()
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitNoResponse(c, stream)
}

func (s *testCoordinatorSuite) TestShouldRun(c *C) {
	tc, co, cleanup := prepare(nil, nil, nil, c)
	defer cleanup()

	c.Assert(tc.addLeaderStore(1, 5), IsNil)
	c.Assert(tc.addLeaderStore(2, 2), IsNil)
	c.Assert(tc.addLeaderStore(3, 0), IsNil)
	c.Assert(tc.addLeaderStore(4, 0), IsNil)
	c.Assert(tc.LoadRegion(1, 1, 2, 3), IsNil)
	c.Assert(tc.LoadRegion(2, 1, 2, 3), IsNil)
	c.Assert(tc.LoadRegion(3, 1, 2, 3), IsNil)
	c.Assert(tc.LoadRegion(4, 1, 2, 3), IsNil)
	c.Assert(tc.LoadRegion(5, 1, 2, 3), IsNil)
	c.Assert(tc.LoadRegion(6, 2, 1, 4), IsNil)
	c.Assert(tc.LoadRegion(7, 2, 1, 4), IsNil)
	c.Assert(co.shouldRun(), IsFalse)
	c.Assert(tc.core.Regions.GetStoreRegionCount(4), Equals, 2)

	tbl := []struct {
		regionID  uint64
		shouldRun bool
	}{
		{1, false},
		{2, false},
		{3, false},
		{4, false},
		{5, false},
		// store4 needs collect two region
		{6, false},
		{7, true},
	}

	for _, t := range tbl {
		r := tc.GetRegion(t.regionID)
		nr := r.Clone(core.WithLeader(r.GetPeers()[0]))
		c.Assert(tc.processRegionHeartbeat(nr), IsNil)
		c.Assert(co.shouldRun(), Equals, t.shouldRun)
	}
	nr := &metapb.Region{Id: 6, Peers: []*metapb.Peer{}}
	newRegion := core.NewRegionInfo(nr, nil)
	c.Assert(tc.processRegionHeartbeat(newRegion), NotNil)
	c.Assert(co.cluster.prepareChecker.sum, Equals, 7)
}

func (s *testCoordinatorSuite) TestShouldRunWithNonLeaderRegions(c *C) {
	tc, co, cleanup := prepare(nil, nil, nil, c)
	defer cleanup()

	c.Assert(tc.addLeaderStore(1, 10), IsNil)
	c.Assert(tc.addLeaderStore(2, 0), IsNil)
	c.Assert(tc.addLeaderStore(3, 0), IsNil)
	for i := 0; i < 10; i++ {
		c.Assert(tc.LoadRegion(uint64(i+1), 1, 2, 3), IsNil)
	}
	c.Assert(co.shouldRun(), IsFalse)
	c.Assert(tc.core.Regions.GetStoreRegionCount(1), Equals, 10)

	tbl := []struct {
		regionID  uint64
		shouldRun bool
	}{
		{1, false},
		{2, false},
		{3, false},
		{4, false},
		{5, false},
		{6, false},
		{7, false},
		{8, true},
	}

	for _, t := range tbl {
		r := tc.GetRegion(t.regionID)
		nr := r.Clone(core.WithLeader(r.GetPeers()[0]))
		c.Assert(tc.processRegionHeartbeat(nr), IsNil)
		c.Assert(co.shouldRun(), Equals, t.shouldRun)
	}
	nr := &metapb.Region{Id: 8, Peers: []*metapb.Peer{}}
	newRegion := core.NewRegionInfo(nr, nil)
	c.Assert(tc.processRegionHeartbeat(newRegion), NotNil)
	c.Assert(co.cluster.prepareChecker.sum, Equals, 8)

	// Now, after server is prepared, there exist some regions with no leader.
	c.Assert(tc.GetRegion(9).GetLeader().GetStoreId(), Equals, uint64(0))
	c.Assert(tc.GetRegion(10).GetLeader().GetStoreId(), Equals, uint64(0))
}

func (s *testCoordinatorSuite) TestAddScheduler(c *C) {
	tc, co, cleanup := prepare(nil, nil, func(co *coordinator) { co.run() }, c)
	defer cleanup()

	c.Assert(co.schedulers, HasLen, 4)
	c.Assert(co.removeScheduler(schedulers.BalanceLeaderName), IsNil)
	c.Assert(co.removeScheduler(schedulers.BalanceRegionName), IsNil)
	c.Assert(co.removeScheduler(schedulers.HotRegionName), IsNil)
	c.Assert(co.removeScheduler(schedulers.LabelName), IsNil)
	c.Assert(co.schedulers, HasLen, 0)

	stream := mockhbstream.NewHeartbeatStream()

	// Add stores 1,2,3
	c.Assert(tc.addLeaderStore(1, 1), IsNil)
	c.Assert(tc.addLeaderStore(2, 1), IsNil)
	c.Assert(tc.addLeaderStore(3, 1), IsNil)
	// Add regions 1 with leader in store 1 and followers in stores 2,3
	c.Assert(tc.addLeaderRegion(1, 1, 2, 3), IsNil)
	// Add regions 2 with leader in store 2 and followers in stores 1,3
	c.Assert(tc.addLeaderRegion(2, 2, 1, 3), IsNil)
	// Add regions 3 with leader in store 3 and followers in stores 1,2
	c.Assert(tc.addLeaderRegion(3, 3, 1, 2), IsNil)

	oc := co.opController
	gls, err := schedule.CreateScheduler(schedulers.GrantLeaderType, oc, core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(schedulers.GrantLeaderType, []string{"0"}))
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(gls), NotNil)
	c.Assert(co.removeScheduler(gls.GetName()), NotNil)

	gls, err = schedule.CreateScheduler(schedulers.GrantLeaderType, oc, core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(schedulers.GrantLeaderType, []string{"1"}))
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(gls), IsNil)

	// Transfer all leaders to store 1.
	waitOperator(c, co, 2)
	region2 := tc.GetRegion(2)
	c.Assert(dispatchHeartbeat(co, region2, stream), IsNil)
	region2 = waitTransferLeader(c, stream, region2, 1)
	c.Assert(dispatchHeartbeat(co, region2, stream), IsNil)
	waitNoResponse(c, stream)

	waitOperator(c, co, 3)
	region3 := tc.GetRegion(3)
	c.Assert(dispatchHeartbeat(co, region3, stream), IsNil)
	region3 = waitTransferLeader(c, stream, region3, 1)
	c.Assert(dispatchHeartbeat(co, region3, stream), IsNil)
	waitNoResponse(c, stream)
}

func (s *testCoordinatorSuite) TestPersistScheduler(c *C) {
	tc, co, cleanup := prepare(nil, nil, func(co *coordinator) { co.run() }, c)
	hbStreams := co.hbStreams
	defer cleanup()

	// Add stores 1,2
	c.Assert(tc.addLeaderStore(1, 1), IsNil)
	c.Assert(tc.addLeaderStore(2, 1), IsNil)

	c.Assert(co.schedulers, HasLen, 4)
	oc := co.opController
	storage := tc.RaftCluster.storage

	gls1, err := schedule.CreateScheduler(schedulers.GrantLeaderType, oc, storage, schedule.ConfigSliceDecoder(schedulers.GrantLeaderType, []string{"1"}))
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(gls1, "1"), IsNil)
	evict, err := schedule.CreateScheduler(schedulers.EvictLeaderType, oc, storage, schedule.ConfigSliceDecoder(schedulers.EvictLeaderType, []string{"2"}))
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(evict, "2"), IsNil)
	c.Assert(co.schedulers, HasLen, 6)
	sches, _, err := storage.LoadAllScheduleConfig()
	c.Assert(err, IsNil)
	c.Assert(sches, HasLen, 6)
	c.Assert(co.removeScheduler(schedulers.BalanceLeaderName), IsNil)
	c.Assert(co.removeScheduler(schedulers.BalanceRegionName), IsNil)
	c.Assert(co.removeScheduler(schedulers.HotRegionName), IsNil)
	c.Assert(co.removeScheduler(schedulers.LabelName), IsNil)
	c.Assert(co.schedulers, HasLen, 2)
	c.Assert(co.cluster.opt.Persist(storage), IsNil)
	co.stop()
	co.wg.Wait()
	// make a new coordinator for testing
	// whether the schedulers added or removed in dynamic way are recorded in opt
	_, newOpt, err := newTestScheduleConfig()
	c.Assert(err, IsNil)
	_, err = schedule.CreateScheduler(schedulers.AdjacentRegionType, oc, storage, schedule.ConfigJSONDecoder([]byte("null")))
	c.Assert(err, IsNil)
	// suppose we add a new default enable scheduler
	newOpt.AddSchedulerCfg(schedulers.AdjacentRegionType, []string{})
	c.Assert(newOpt.GetSchedulers(), HasLen, 5)
	c.Assert(newOpt.Reload(storage), IsNil)
	// only remains 3 items with independent config.
	sches, _, err = storage.LoadAllScheduleConfig()
	c.Assert(err, IsNil)
	c.Assert(sches, HasLen, 3)

	// option have 7 items because the default scheduler do not remove.
	c.Assert(newOpt.GetSchedulers(), HasLen, 7)
	c.Assert(newOpt.Persist(storage), IsNil)
	tc.RaftCluster.opt = newOpt

	co = newCoordinator(s.ctx, tc.RaftCluster, hbStreams)
	co.run()
	c.Assert(co.schedulers, HasLen, 3)
	co.stop()
	co.wg.Wait()
	// suppose restart PD again
	_, newOpt, err = newTestScheduleConfig()
	c.Assert(err, IsNil)
	c.Assert(newOpt.Reload(storage), IsNil)
	tc.RaftCluster.opt = newOpt
	co = newCoordinator(s.ctx, tc.RaftCluster, hbStreams)
	co.run()
	storage = tc.RaftCluster.storage
	c.Assert(co.schedulers, HasLen, 3)
	bls, err := schedule.CreateScheduler(schedulers.BalanceLeaderType, oc, storage, schedule.ConfigSliceDecoder(schedulers.BalanceLeaderType, []string{"", ""}))
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(bls), IsNil)
	brs, err := schedule.CreateScheduler(schedulers.BalanceRegionType, oc, storage, schedule.ConfigSliceDecoder(schedulers.BalanceRegionType, []string{"", ""}))
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(brs), IsNil)
	c.Assert(co.schedulers, HasLen, 5)

	// the scheduler option should contain 7 items
	// the `hot scheduler` and `label scheduler` are disabled
	c.Assert(co.cluster.opt.GetSchedulers(), HasLen, 7)
	c.Assert(co.removeScheduler(schedulers.GrantLeaderName), IsNil)
	// the scheduler that is not enable by default will be completely deleted
	c.Assert(co.cluster.opt.GetSchedulers(), HasLen, 6)
	c.Assert(co.schedulers, HasLen, 4)
	c.Assert(co.cluster.opt.Persist(co.cluster.storage), IsNil)
	co.stop()
	co.wg.Wait()
	_, newOpt, err = newTestScheduleConfig()
	c.Assert(err, IsNil)
	c.Assert(newOpt.Reload(co.cluster.storage), IsNil)
	tc.RaftCluster.opt = newOpt
	co = newCoordinator(s.ctx, tc.RaftCluster, hbStreams)

	co.run()
	c.Assert(co.schedulers, HasLen, 4)
	c.Assert(co.removeScheduler(schedulers.EvictLeaderName), IsNil)
	c.Assert(co.schedulers, HasLen, 3)
}

func (s *testCoordinatorSuite) TestRemoveScheduler(c *C) {
	tc, co, cleanup := prepare(func(cfg *config.ScheduleConfig) {
		cfg.ReplicaScheduleLimit = 0
	}, nil, func(co *coordinator) { co.run() }, c)
	hbStreams := co.hbStreams
	defer cleanup()

	// Add stores 1,2
	c.Assert(tc.addLeaderStore(1, 1), IsNil)
	c.Assert(tc.addLeaderStore(2, 1), IsNil)

	c.Assert(co.schedulers, HasLen, 4)
	oc := co.opController
	storage := tc.RaftCluster.storage

	gls1, err := schedule.CreateScheduler(schedulers.GrantLeaderType, oc, storage, schedule.ConfigSliceDecoder(schedulers.GrantLeaderType, []string{"1"}))
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(gls1, "1"), IsNil)
	c.Assert(co.schedulers, HasLen, 5)
	sches, _, err := storage.LoadAllScheduleConfig()
	c.Assert(err, IsNil)
	c.Assert(sches, HasLen, 5)

	// remove all schedulers
	c.Assert(co.removeScheduler(schedulers.BalanceLeaderName), IsNil)
	c.Assert(co.removeScheduler(schedulers.BalanceRegionName), IsNil)
	c.Assert(co.removeScheduler(schedulers.HotRegionName), IsNil)
	c.Assert(co.removeScheduler(schedulers.LabelName), IsNil)
	c.Assert(co.removeScheduler(schedulers.GrantLeaderName), IsNil)
	// all removed
	sches, _, err = storage.LoadAllScheduleConfig()
	c.Assert(err, IsNil)
	c.Assert(sches, HasLen, 0)
	c.Assert(co.schedulers, HasLen, 0)
	c.Assert(co.cluster.opt.Persist(co.cluster.storage), IsNil)
	co.stop()
	co.wg.Wait()

	// suppose restart PD again
	_, newOpt, err := newTestScheduleConfig()
	c.Assert(err, IsNil)
	c.Assert(newOpt.Reload(tc.storage), IsNil)
	tc.RaftCluster.opt = newOpt
	co = newCoordinator(s.ctx, tc.RaftCluster, hbStreams)
	co.run()
	c.Assert(co.schedulers, HasLen, 0)
	// the option remains default scheduler
	c.Assert(co.cluster.opt.GetSchedulers(), HasLen, 4)
	co.stop()
	co.wg.Wait()
}

func (s *testCoordinatorSuite) TestRestart(c *C) {
	tc, co, cleanup := prepare(func(cfg *config.ScheduleConfig) {
		// Turn off balance, we test add replica only.
		cfg.LeaderScheduleLimit = 0
		cfg.RegionScheduleLimit = 0
	}, nil, func(co *coordinator) { co.run() }, c)
	hbStreams := co.hbStreams
	defer cleanup()

	// Add 3 stores (1, 2, 3) and a region with 1 replica on store 1.
	c.Assert(tc.addRegionStore(1, 1), IsNil)
	c.Assert(tc.addRegionStore(2, 2), IsNil)
	c.Assert(tc.addRegionStore(3, 3), IsNil)
	c.Assert(tc.addLeaderRegion(1, 1), IsNil)
	region := tc.GetRegion(1)
	tc.prepareChecker.collect(region)

	// Add 1 replica on store 2.
	co = newCoordinator(s.ctx, tc.RaftCluster, hbStreams)
	co.run()
	stream := mockhbstream.NewHeartbeatStream()
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitAddLearner(c, stream, region, 2)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitPromoteLearner(c, stream, region, 2)
	co.stop()
	co.wg.Wait()

	// Recreate coodinator then add another replica on store 3.
	co = newCoordinator(s.ctx, tc.RaftCluster, hbStreams)
	co.run()
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	region = waitAddLearner(c, stream, region, 3)
	c.Assert(dispatchHeartbeat(co, region, stream), IsNil)
	waitPromoteLearner(c, stream, region, 3)
}

func BenchmarkPatrolRegion(b *testing.B) {
	mergeLimit := uint64(4100)
	regionNum := 10000

	tc, co, cleanup := prepare(func(cfg *config.ScheduleConfig) {
		cfg.MergeScheduleLimit = mergeLimit
	}, nil, nil, &C{})
	defer cleanup()

	tc.opt.SetSplitMergeInterval(time.Duration(0))
	for i := 1; i < 4; i++ {
		if err := tc.addRegionStore(uint64(i), regionNum, 96); err != nil {
			return
		}
	}
	for i := 0; i < regionNum; i++ {
		if err := tc.addLeaderRegion(uint64(i), 1, 2, 3); err != nil {
			return
		}
	}

	listen := make(chan int)
	go func() {
		oc := co.opController
		listen <- 0
		for {
			if oc.OperatorCount(operator.OpMerge) == mergeLimit {
				co.cancel()
				co.wg.Add(1)
				return
			}
		}
	}()
	<-listen

	b.ResetTimer()
	co.patrolRegions()
}

func waitOperator(c *C, co *coordinator, regionID uint64) {
	testutil.WaitUntil(c, func(c *C) bool {
		return co.opController.GetOperator(regionID) != nil
	})
}

var _ = Suite(&testOperatorControllerSuite{})

type testOperatorControllerSuite struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *testOperatorControllerSuite) SetUpSuite(c *C) {
	s.ctx, s.cancel = context.WithCancel(context.Background())
}

func (s *testOperatorControllerSuite) TearDownSuite(c *C) {
	s.cancel()
}

func (s *testOperatorControllerSuite) TestOperatorCount(c *C) {
	tc, co, cleanup := prepare(nil, nil, nil, c)
	defer cleanup()
	oc := co.opController
	c.Assert(oc.OperatorCount(operator.OpLeader), Equals, uint64(0))
	c.Assert(oc.OperatorCount(operator.OpRegion), Equals, uint64(0))

	c.Assert(tc.addLeaderRegion(1, 1), IsNil)
	c.Assert(tc.addLeaderRegion(2, 2), IsNil)
	op1 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), operator.OpLeader)
	oc.AddWaitingOperator(op1)
	c.Assert(oc.OperatorCount(operator.OpLeader), Equals, uint64(1)) // 1:leader
	op2 := newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), operator.OpLeader)
	oc.AddWaitingOperator(op2)
	c.Assert(oc.OperatorCount(operator.OpLeader), Equals, uint64(2)) // 1:leader, 2:leader
	c.Assert(oc.RemoveOperator(op1), IsTrue)
	c.Assert(oc.OperatorCount(operator.OpLeader), Equals, uint64(1)) // 2:leader

	op1 = newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), operator.OpRegion)
	oc.AddWaitingOperator(op1)
	c.Assert(oc.OperatorCount(operator.OpRegion), Equals, uint64(1)) // 1:region 2:leader
	c.Assert(oc.OperatorCount(operator.OpLeader), Equals, uint64(1))
	op2 = newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), operator.OpRegion)
	op2.SetPriorityLevel(core.HighPriority)
	oc.AddWaitingOperator(op2)
	c.Assert(oc.OperatorCount(operator.OpRegion), Equals, uint64(2)) // 1:region 2:region
	c.Assert(oc.OperatorCount(operator.OpLeader), Equals, uint64(0))
}

func (s *testOperatorControllerSuite) TestStoreOverloaded(c *C) {
	tc, co, cleanup := prepare(func(cfg *config.ScheduleConfig) {
		// scheduling one time needs 60 seconds
		// and thus it's large enough to make sure that only schedule one time
		cfg.StoreBalanceRate = 1
	}, nil, nil, c)
	defer cleanup()
	oc := co.opController
	lb, err := schedule.CreateScheduler(schedulers.BalanceRegionType, oc, tc.storage, schedule.ConfigSliceDecoder(schedulers.BalanceRegionType, []string{"", ""}))
	c.Assert(err, IsNil)
	c.Assert(tc.addRegionStore(4, 100), IsNil)
	c.Assert(tc.addRegionStore(3, 100), IsNil)
	c.Assert(tc.addRegionStore(2, 100), IsNil)
	c.Assert(tc.addRegionStore(1, 10), IsNil)
	c.Assert(tc.addLeaderRegion(1, 2, 3, 4), IsNil)
	region := tc.GetRegion(1).Clone(core.SetApproximateSize(60))
	tc.putRegion(region)
	op1 := lb.Schedule(tc)[0]
	c.Assert(op1, NotNil)
	c.Assert(oc.AddOperator(op1), IsTrue)
	c.Assert(oc.RemoveOperator(op1), IsTrue)
	for i := 0; i < 10; i++ {
		c.Assert(lb.Schedule(tc), IsNil)
	}

	// reset all stores' limit
	// scheduling one time needs 1/10 seconds
	oc.SetAllStoresLimit(10)
	for i := 0; i < 10; i++ {
		op1 = lb.Schedule(tc)[0]
		c.Assert(op1, NotNil)
		c.Assert(oc.AddOperator(op1), IsTrue)
		c.Assert(oc.RemoveOperator(op1), IsTrue)
	}
	// sleep 1 seconds to make sure that the token is filled up
	time.Sleep(1 * time.Second)
	for i := 0; i < 100; i++ {
		c.Assert(lb.Schedule(tc), NotNil)
	}
}

func (s *testOperatorControllerSuite) TestStoreOverloadedWithReplace(c *C) {
	tc, co, cleanup := prepare(func(cfg *config.ScheduleConfig) {
		// scheduling one time needs 2 seconds
		cfg.StoreBalanceRate = 30
	}, nil, nil, c)
	defer cleanup()
	oc := co.opController
	lb, err := schedule.CreateScheduler(schedulers.BalanceRegionType, oc, tc.storage, schedule.ConfigSliceDecoder(schedulers.BalanceRegionType, []string{"", ""}))
	c.Assert(err, IsNil)

	c.Assert(tc.addRegionStore(4, 100), IsNil)
	c.Assert(tc.addRegionStore(3, 100), IsNil)
	c.Assert(tc.addRegionStore(2, 100), IsNil)
	c.Assert(tc.addRegionStore(1, 10), IsNil)
	c.Assert(tc.addLeaderRegion(1, 2, 3, 4), IsNil)
	c.Assert(tc.addLeaderRegion(2, 1, 3, 4), IsNil)
	region := tc.GetRegion(1).Clone(core.SetApproximateSize(60))
	tc.putRegion(region)
	region = tc.GetRegion(2).Clone(core.SetApproximateSize(60))
	tc.putRegion(region)
	op1 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), operator.OpRegion, operator.AddPeer{ToStore: 1, PeerID: 1})
	c.Assert(oc.AddOperator(op1), IsTrue)
	op2 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), operator.OpRegion, operator.AddPeer{ToStore: 2, PeerID: 2})
	op2.SetPriorityLevel(core.HighPriority)
	c.Assert(oc.AddOperator(op2), IsTrue)
	op3 := newTestOperator(1, tc.GetRegion(2).GetRegionEpoch(), operator.OpRegion, operator.AddPeer{ToStore: 1, PeerID: 3})
	c.Assert(oc.AddOperator(op3), IsFalse)
	c.Assert(lb.Schedule(tc), IsNil)
	// sleep 2 seconds to make sure that token is filled up
	time.Sleep(2 * time.Second)
	c.Assert(lb.Schedule(tc), NotNil)
}

var _ = Suite(&testScheduleControllerSuite{})

type testScheduleControllerSuite struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *testScheduleControllerSuite) SetUpSuite(c *C) {
	s.ctx, s.cancel = context.WithCancel(context.Background())
}

func (s *testScheduleControllerSuite) TearDownSuite(c *C) {
	s.cancel()
}

// FIXME: remove after move into schedulers package
type mockLimitScheduler struct {
	schedule.Scheduler
	limit   uint64
	counter *schedule.OperatorController
	kind    operator.OpKind
}

func (s *mockLimitScheduler) IsScheduleAllowed(cluster opt.Cluster) bool {
	return s.counter.OperatorCount(s.kind) < s.limit
}

func (s *testScheduleControllerSuite) TestController(c *C) {
	tc, co, cleanup := prepare(nil, nil, nil, c)
	defer cleanup()
	oc := co.opController

	c.Assert(tc.addLeaderRegion(1, 1), IsNil)
	c.Assert(tc.addLeaderRegion(2, 2), IsNil)
	scheduler, err := schedule.CreateScheduler(schedulers.BalanceLeaderType, oc, core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(schedulers.BalanceLeaderType, []string{"", ""}))
	c.Assert(err, IsNil)
	lb := &mockLimitScheduler{
		Scheduler: scheduler,
		counter:   oc,
		kind:      operator.OpLeader,
	}

	sc := newScheduleController(co, lb)

	for i := schedulers.MinScheduleInterval; sc.GetInterval() != schedulers.MaxScheduleInterval; i = sc.GetNextInterval(i) {
		c.Assert(sc.GetInterval(), Equals, i)
		c.Assert(sc.Schedule(), IsNil)
	}
	// limit = 2
	lb.limit = 2
	// count = 0
	c.Assert(sc.AllowSchedule(), IsTrue)
	op1 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), operator.OpLeader)
	c.Assert(oc.AddWaitingOperator(op1), IsTrue)
	// count = 1
	c.Assert(sc.AllowSchedule(), IsTrue)
	op2 := newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), operator.OpLeader)
	c.Assert(oc.AddWaitingOperator(op2), IsTrue)
	// count = 2
	c.Assert(sc.AllowSchedule(), IsFalse)
	c.Assert(oc.RemoveOperator(op1), IsTrue)
	// count = 1
	c.Assert(sc.AllowSchedule(), IsTrue)

	// add a PriorityKind operator will remove old operator
	op3 := newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), operator.OpHotRegion)
	op3.SetPriorityLevel(core.HighPriority)
	c.Assert(oc.AddWaitingOperator(op1), IsTrue)
	c.Assert(sc.AllowSchedule(), IsFalse)
	c.Assert(oc.AddWaitingOperator(op3), IsTrue)
	c.Assert(sc.AllowSchedule(), IsTrue)
	c.Assert(oc.RemoveOperator(op3), IsTrue)

	// add a admin operator will remove old operator
	c.Assert(oc.AddWaitingOperator(op2), IsTrue)
	c.Assert(sc.AllowSchedule(), IsFalse)
	op4 := newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), operator.OpAdmin)
	op4.SetPriorityLevel(core.HighPriority)
	c.Assert(oc.AddWaitingOperator(op4), IsTrue)
	c.Assert(sc.AllowSchedule(), IsTrue)
	c.Assert(oc.RemoveOperator(op4), IsTrue)

	// test wrong region id.
	op5 := newTestOperator(3, &metapb.RegionEpoch{}, operator.OpHotRegion)
	c.Assert(oc.AddWaitingOperator(op5), IsFalse)

	// test wrong region epoch.
	c.Assert(oc.RemoveOperator(op1), IsTrue)
	epoch := &metapb.RegionEpoch{
		Version: tc.GetRegion(1).GetRegionEpoch().GetVersion() + 1,
		ConfVer: tc.GetRegion(1).GetRegionEpoch().GetConfVer(),
	}
	op6 := newTestOperator(1, epoch, operator.OpLeader)
	c.Assert(oc.AddWaitingOperator(op6), IsFalse)
	epoch.Version--
	op6 = newTestOperator(1, epoch, operator.OpLeader)
	c.Assert(oc.AddWaitingOperator(op6), IsTrue)
	c.Assert(oc.RemoveOperator(op6), IsTrue)
}

func (s *testScheduleControllerSuite) TestInterval(c *C) {
	_, co, cleanup := prepare(nil, nil, nil, c)
	defer cleanup()

	lb, err := schedule.CreateScheduler(schedulers.BalanceLeaderType, co.opController, core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(schedulers.BalanceLeaderType, []string{"", ""}))
	c.Assert(err, IsNil)
	sc := newScheduleController(co, lb)

	// If no operator for x seconds, the next check should be in x/2 seconds.
	idleSeconds := []int{5, 10, 20, 30, 60}
	for _, n := range idleSeconds {
		sc.nextInterval = schedulers.MinScheduleInterval
		for totalSleep := time.Duration(0); totalSleep <= time.Second*time.Duration(n); totalSleep += sc.GetInterval() {
			c.Assert(sc.Schedule(), IsNil)
		}
		c.Assert(sc.GetInterval(), Less, time.Second*time.Duration(n/2))
	}
}

func waitAddLearner(c *C, stream mockhbstream.HeartbeatStream, region *core.RegionInfo, storeID uint64) *core.RegionInfo {
	var res *pdpb.RegionHeartbeatResponse
	testutil.WaitUntil(c, func(c *C) bool {
		if res = stream.Recv(); res != nil {
			return res.GetRegionId() == region.GetID() &&
				res.GetChangePeer().GetChangeType() == eraftpb.ConfChangeType_AddLearnerNode &&
				res.GetChangePeer().GetPeer().GetStoreId() == storeID
		}
		return false
	})
	return region.Clone(
		core.WithAddPeer(res.GetChangePeer().GetPeer()),
		core.WithIncConfVer(),
	)
}

func waitPromoteLearner(c *C, stream mockhbstream.HeartbeatStream, region *core.RegionInfo, storeID uint64) *core.RegionInfo {
	var res *pdpb.RegionHeartbeatResponse
	testutil.WaitUntil(c, func(c *C) bool {
		if res = stream.Recv(); res != nil {
			return res.GetRegionId() == region.GetID() &&
				res.GetChangePeer().GetChangeType() == eraftpb.ConfChangeType_AddNode &&
				res.GetChangePeer().GetPeer().GetStoreId() == storeID
		}
		return false
	})
	// Remove learner than add voter.
	return region.Clone(
		core.WithRemoveStorePeer(storeID),
		core.WithAddPeer(res.GetChangePeer().GetPeer()),
	)
}

func waitRemovePeer(c *C, stream mockhbstream.HeartbeatStream, region *core.RegionInfo, storeID uint64) *core.RegionInfo {
	var res *pdpb.RegionHeartbeatResponse
	testutil.WaitUntil(c, func(c *C) bool {
		if res = stream.Recv(); res != nil {
			return res.GetRegionId() == region.GetID() &&
				res.GetChangePeer().GetChangeType() == eraftpb.ConfChangeType_RemoveNode &&
				res.GetChangePeer().GetPeer().GetStoreId() == storeID
		}
		return false
	})
	return region.Clone(
		core.WithRemoveStorePeer(storeID),
		core.WithIncConfVer(),
	)
}

func waitTransferLeader(c *C, stream mockhbstream.HeartbeatStream, region *core.RegionInfo, storeID uint64) *core.RegionInfo {
	var res *pdpb.RegionHeartbeatResponse
	testutil.WaitUntil(c, func(c *C) bool {
		if res = stream.Recv(); res != nil {
			return res.GetRegionId() == region.GetID() && res.GetTransferLeader().GetPeer().GetStoreId() == storeID
		}
		return false
	})
	return region.Clone(
		core.WithLeader(res.GetTransferLeader().GetPeer()),
	)
}

func waitNoResponse(c *C, stream mockhbstream.HeartbeatStream) {
	testutil.WaitUntil(c, func(c *C) bool {
		res := stream.Recv()
		return res == nil
	})
}

func getHeartBeatStreams(ctx context.Context, c *C, tc *testCluster) (*heartbeatStreams, func()) {
	config := NewTestSingleConfig(c)
	svr, err := CreateServer(config)
	c.Assert(err, IsNil)
	kvBase := kv.NewEtcdKVBase(svr.client, svr.rootPath)
	path := filepath.Join(svr.cfg.DataDir, "region-meta")
	regionStorage, err := core.NewRegionStorage(ctx, path)
	c.Assert(err, IsNil)
	svr.storage = core.NewStorage(kvBase).SetRegionStorage(regionStorage)
	cluster := tc.RaftCluster
	cluster.s = svr
	cluster.running = false
	cluster.clusterID = tc.getClusterID()
	cluster.clusterRoot = svr.getClusterRootPath()
	cluster.regionSyncer = syncer.NewRegionSyncer(svr)
	hbStreams := newHeartbeatStreams(ctx, tc.getClusterID(), cluster)
	return hbStreams, func() { testutil.CleanServer(config.DataDir) }
}

func createTestRaftCluster(id id.Allocator, opt *config.ScheduleOption, storage *core.Storage) *RaftCluster {
	cluster := &RaftCluster{}
	cluster.initCluster(id, opt, storage)
	return cluster
}
