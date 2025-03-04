// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package shard

import (
	"context"
	"sync"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/server/api/historyservice/v1"

	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common"
	"go.temporal.io/server/common/backoff"
	"go.temporal.io/server/common/clock"
	"go.temporal.io/server/common/convert"
	"go.temporal.io/server/common/definition"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/primitives/timestamp"
	"go.temporal.io/server/common/resource"
	"go.temporal.io/server/service/history/configs"
	"go.temporal.io/server/service/history/events"
	"go.temporal.io/server/service/history/tasks"
)

var (
	defaultTime = time.Unix(0, 0)

	persistenceOperationRetryPolicy = common.CreatePersistenceRetryPolicy()
)

const (
	// See transitionLocked for overview of state transitions.

	// These are the possible values of ContextImpl.state:
	contextStateInitialized contextState = iota
	contextStateAcquiring
	contextStateAcquired
	contextStateStopping
	contextStateStopped

	// These are the requests that can be passed to transitionLocked to change state:
	contextRequestAcquire contextRequest = iota
	contextRequestAcquired
	contextRequestLost
	contextRequestStop
	contextRequestFinishStop
)

type (
	contextState   int32
	contextRequest int

	ContextImpl struct {
		// These fields are constant:
		resource.Resource
		shardID          int32
		executionManager persistence.ExecutionManager
		metricsClient    metrics.Client
		eventsCache      events.Cache
		closeCallback    func(*ContextImpl)
		config           *configs.Config
		logger           log.Logger
		throttledLogger  log.Logger
		engineFactory    EngineFactory

		// All following fields are protected by rwLock, and only valid if state >= Acquiring:
		rwLock                    sync.RWMutex
		state                     contextState
		engine                    Engine
		lastUpdated               time.Time
		shardInfo                 *persistence.ShardInfoWithFailover
		transferSequenceNumber    int64
		maxTransferSequenceNumber int64
		transferMaxReadLevel      int64
		timerMaxReadLevelMap      map[string]time.Time // cluster -> timerMaxReadLevel

		// exist only in memory
		remoteClusterInfos map[string]*remoteClusterInfo
	}

	remoteClusterInfo struct {
		CurrentTime               time.Time
		AckedReplicationTaskID    int64
		AckedReplicationTimestamp time.Time
	}
)

var _ Context = (*ContextImpl)(nil)

var (
	// ErrShardClosed is returned when shard is closed and a req cannot be processed
	ErrShardClosed = serviceerror.NewUnavailable("shard closed")

	// ErrShardStatusUnknown means we're not sure if we have the shard lock or not. This may be returned
	// during short windows at initialization and if we've lost the connection to the database.
	ErrShardStatusUnknown = serviceerror.NewUnavailable("shard status unknown")

	// errStoppingContext is an internal error used to abort acquireShard
	errStoppingContext = serviceerror.NewUnavailable("stopping context")
)

const (
	logWarnTransferLevelDiff = 3000000 // 3 million
	logWarnTimerLevelDiff    = time.Duration(30 * time.Minute)
	historySizeLogThreshold  = 10 * 1024 * 1024
)

func (s *ContextImpl) GetShardID() int32 {
	// constant from initialization, no need for locks
	return s.shardID
}

func (s *ContextImpl) GetService() resource.Resource {
	// constant from initialization, no need for locks
	return s.Resource
}

func (s *ContextImpl) GetExecutionManager() persistence.ExecutionManager {
	// constant from initialization, no need for locks
	return s.executionManager
}

func (s *ContextImpl) GetEngine() (Engine, error) {
	s.rLock()
	defer s.rUnlock()

	if err := s.errorByStateLocked(); err != nil {
		return nil, err
	}

	return s.engine, nil
}

func (s *ContextImpl) GenerateTransferTaskID() (int64, error) {
	s.wLock()
	defer s.wUnlock()

	return s.generateTransferTaskIDLocked()
}

func (s *ContextImpl) GenerateTransferTaskIDs(number int) ([]int64, error) {
	s.wLock()
	defer s.wUnlock()

	result := []int64{}
	for i := 0; i < number; i++ {
		id, err := s.generateTransferTaskIDLocked()
		if err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, nil
}

func (s *ContextImpl) GetTransferMaxReadLevel() int64 {
	s.rLock()
	defer s.rUnlock()
	return s.transferMaxReadLevel
}

func (s *ContextImpl) GetTransferAckLevel() int64 {
	s.rLock()
	defer s.rUnlock()

	return s.shardInfo.TransferAckLevel
}

func (s *ContextImpl) UpdateTransferAckLevel(ackLevel int64) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.TransferAckLevel = ackLevel
	s.shardInfo.StolenSinceRenew = 0
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetTransferClusterAckLevel(cluster string) int64 {
	s.rLock()
	defer s.rUnlock()

	// if we can find corresponding ack level
	if ackLevel, ok := s.shardInfo.ClusterTransferAckLevel[cluster]; ok {
		return ackLevel
	}
	// otherwise, default to existing ack level, which belongs to local cluster
	// this can happen if you add more cluster
	return s.shardInfo.TransferAckLevel
}

func (s *ContextImpl) UpdateTransferClusterAckLevel(cluster string, ackLevel int64) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.ClusterTransferAckLevel[cluster] = ackLevel
	s.shardInfo.StolenSinceRenew = 0
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetVisibilityAckLevel() int64 {
	s.rLock()
	defer s.rUnlock()

	return s.shardInfo.VisibilityAckLevel
}

func (s *ContextImpl) UpdateVisibilityAckLevel(ackLevel int64) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.VisibilityAckLevel = ackLevel
	s.shardInfo.StolenSinceRenew = 0
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetTieredStorageAckLevel() int64 {
	s.rLock()
	defer s.rUnlock()

	return s.shardInfo.TieredStorageAckLevel
}

func (s *ContextImpl) UpdateTieredStorageAckLevel(ackLevel int64) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.TieredStorageAckLevel = ackLevel
	s.shardInfo.StolenSinceRenew = 0
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetReplicatorAckLevel() int64 {
	s.rLock()
	defer s.rUnlock()

	return s.shardInfo.ReplicationAckLevel
}

func (s *ContextImpl) UpdateReplicatorAckLevel(ackLevel int64) error {
	s.wLock()
	defer s.wUnlock()
	s.shardInfo.ReplicationAckLevel = ackLevel
	s.shardInfo.StolenSinceRenew = 0
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetReplicatorDLQAckLevel(sourceCluster string) int64 {
	s.rLock()
	defer s.rUnlock()

	if ackLevel, ok := s.shardInfo.ReplicationDlqAckLevel[sourceCluster]; ok {
		return ackLevel
	}
	return -1
}

func (s *ContextImpl) UpdateReplicatorDLQAckLevel(
	sourceCluster string,
	ackLevel int64,
) error {

	s.wLock()
	defer s.wUnlock()

	s.shardInfo.ReplicationDlqAckLevel[sourceCluster] = ackLevel
	s.shardInfo.StolenSinceRenew = 0
	if err := s.updateShardInfoLocked(); err != nil {
		return err
	}

	s.GetMetricsClient().Scope(
		metrics.ReplicationDLQStatsScope,
		metrics.TargetClusterTag(sourceCluster),
		metrics.InstanceTag(convert.Int32ToString(s.shardID)),
	).UpdateGauge(
		metrics.ReplicationDLQAckLevelGauge,
		float64(ackLevel),
	)
	return nil
}

func (s *ContextImpl) GetClusterReplicationLevel(cluster string) int64 {
	s.rLock()
	defer s.rUnlock()

	// if we can find corresponding replication level
	if replicationLevel, ok := s.shardInfo.ClusterReplicationLevel[cluster]; ok {
		return replicationLevel
	}

	// New cluster always starts from -1
	return persistence.EmptyQueueMessageID
}

func (s *ContextImpl) UpdateClusterReplicationLevel(cluster string, ackTaskID int64, ackTimestamp time.Time) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.ClusterReplicationLevel[cluster] = ackTaskID
	s.shardInfo.StolenSinceRenew = 0
	s.getRemoteClusterInfoLocked(cluster).AckedReplicationTaskID = ackTaskID
	s.getRemoteClusterInfoLocked(cluster).AckedReplicationTimestamp = ackTimestamp
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetTimerAckLevel() time.Time {
	s.rLock()
	defer s.rUnlock()

	return timestamp.TimeValue(s.shardInfo.TimerAckLevelTime)
}

func (s *ContextImpl) UpdateTimerAckLevel(ackLevel time.Time) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.TimerAckLevelTime = &ackLevel
	s.shardInfo.StolenSinceRenew = 0
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetTimerClusterAckLevel(cluster string) time.Time {
	s.rLock()
	defer s.rUnlock()

	// if we can find corresponding ack level
	if ackLevel, ok := s.shardInfo.ClusterTimerAckLevel[cluster]; ok {
		return timestamp.TimeValue(ackLevel)
	}
	// otherwise, default to existing ack level, which belongs to local cluster
	// this can happen if you add more cluster
	return timestamp.TimeValue(s.shardInfo.TimerAckLevelTime)
}

func (s *ContextImpl) UpdateTimerClusterAckLevel(cluster string, ackLevel time.Time) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.ClusterTimerAckLevel[cluster] = &ackLevel
	s.shardInfo.StolenSinceRenew = 0
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) UpdateTransferFailoverLevel(failoverID string, level persistence.TransferFailoverLevel) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.TransferFailoverLevels[failoverID] = level
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) DeleteTransferFailoverLevel(failoverID string) error {
	s.wLock()
	defer s.wUnlock()

	if level, ok := s.shardInfo.TransferFailoverLevels[failoverID]; ok {
		s.GetMetricsClient().RecordTimer(metrics.ShardInfoScope, metrics.ShardInfoTransferFailoverLatencyTimer, time.Since(level.StartTime))
		delete(s.shardInfo.TransferFailoverLevels, failoverID)
	}
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetAllTransferFailoverLevels() map[string]persistence.TransferFailoverLevel {
	s.rLock()
	defer s.rUnlock()

	ret := map[string]persistence.TransferFailoverLevel{}
	for k, v := range s.shardInfo.TransferFailoverLevels {
		ret[k] = v
	}
	return ret
}

func (s *ContextImpl) UpdateTimerFailoverLevel(failoverID string, level persistence.TimerFailoverLevel) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.TimerFailoverLevels[failoverID] = level
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) DeleteTimerFailoverLevel(failoverID string) error {
	s.wLock()
	defer s.wUnlock()

	if level, ok := s.shardInfo.TimerFailoverLevels[failoverID]; ok {
		s.GetMetricsClient().RecordTimer(metrics.ShardInfoScope, metrics.ShardInfoTimerFailoverLatencyTimer, time.Since(level.StartTime))
		delete(s.shardInfo.TimerFailoverLevels, failoverID)
	}
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetAllTimerFailoverLevels() map[string]persistence.TimerFailoverLevel {
	s.rLock()
	defer s.rUnlock()

	ret := map[string]persistence.TimerFailoverLevel{}
	for k, v := range s.shardInfo.TimerFailoverLevels {
		ret[k] = v
	}
	return ret
}

func (s *ContextImpl) GetNamespaceNotificationVersion() int64 {
	s.rLock()
	defer s.rUnlock()

	return s.shardInfo.NamespaceNotificationVersion
}

func (s *ContextImpl) UpdateNamespaceNotificationVersion(namespaceNotificationVersion int64) error {
	s.wLock()
	defer s.wUnlock()

	s.shardInfo.NamespaceNotificationVersion = namespaceNotificationVersion
	return s.updateShardInfoLocked()
}

func (s *ContextImpl) GetTimerMaxReadLevel(cluster string) time.Time {
	s.rLock()
	defer s.rUnlock()

	return s.timerMaxReadLevelMap[cluster]
}

func (s *ContextImpl) UpdateTimerMaxReadLevel(cluster string) time.Time {
	s.wLock()
	defer s.wUnlock()

	currentTime := s.GetTimeSource().Now()
	if cluster != "" && cluster != s.GetClusterMetadata().GetCurrentClusterName() {
		currentTime = s.getRemoteClusterInfoLocked(cluster).CurrentTime
	}

	s.timerMaxReadLevelMap[cluster] = currentTime.Add(s.config.TimerProcessorMaxTimeShift()).Truncate(time.Millisecond)
	return s.timerMaxReadLevelMap[cluster]
}

func (s *ContextImpl) CreateWorkflowExecution(
	request *persistence.CreateWorkflowExecutionRequest,
) (*persistence.CreateWorkflowExecutionResponse, error) {
	if err := s.errorByState(); err != nil {
		return nil, err
	}

	namespaceID := namespace.ID(request.NewWorkflowSnapshot.ExecutionInfo.NamespaceId)
	workflowID := request.NewWorkflowSnapshot.ExecutionInfo.WorkflowId

	// do not try to get namespace cache within shard lock
	namespaceEntry, err := s.GetNamespaceRegistry().GetNamespaceByID(namespaceID)
	if err != nil {
		return nil, err
	}

	s.wLock()
	defer s.wUnlock()

	transferMaxReadLevel := int64(0)
	if err := s.allocateTaskIDsLocked(
		namespaceEntry,
		workflowID,
		request.NewWorkflowSnapshot.TransferTasks,
		request.NewWorkflowSnapshot.ReplicationTasks,
		request.NewWorkflowSnapshot.TimerTasks,
		request.NewWorkflowSnapshot.VisibilityTasks,
		&transferMaxReadLevel,
	); err != nil {
		return nil, err
	}
	defer s.updateMaxReadLevelLocked(transferMaxReadLevel)

	currentRangeID := s.getRangeIDLocked()
	request.RangeID = currentRangeID
	resp, err := s.executionManager.CreateWorkflowExecution(request)
	if err = s.handleErrorLocked(err); err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *ContextImpl) UpdateWorkflowExecution(
	request *persistence.UpdateWorkflowExecutionRequest,
) (*persistence.UpdateWorkflowExecutionResponse, error) {
	if err := s.errorByState(); err != nil {
		return nil, err
	}

	namespaceID := namespace.ID(request.UpdateWorkflowMutation.ExecutionInfo.NamespaceId)
	workflowID := request.UpdateWorkflowMutation.ExecutionInfo.WorkflowId

	// do not try to get namespace cache within shard lock
	namespaceEntry, err := s.GetNamespaceRegistry().GetNamespaceByID(namespaceID)
	if err != nil {
		return nil, err
	}

	s.wLock()
	defer s.wUnlock()

	transferMaxReadLevel := int64(0)
	if err := s.allocateTaskIDsLocked(
		namespaceEntry,
		workflowID,
		request.UpdateWorkflowMutation.TransferTasks,
		request.UpdateWorkflowMutation.ReplicationTasks,
		request.UpdateWorkflowMutation.TimerTasks,
		request.UpdateWorkflowMutation.VisibilityTasks,
		&transferMaxReadLevel,
	); err != nil {
		return nil, err
	}
	if request.NewWorkflowSnapshot != nil {
		if err := s.allocateTaskIDsLocked(
			namespaceEntry,
			workflowID,
			request.NewWorkflowSnapshot.TransferTasks,
			request.NewWorkflowSnapshot.ReplicationTasks,
			request.NewWorkflowSnapshot.TimerTasks,
			request.NewWorkflowSnapshot.VisibilityTasks,
			&transferMaxReadLevel,
		); err != nil {
			return nil, err
		}
	}
	defer s.updateMaxReadLevelLocked(transferMaxReadLevel)

	currentRangeID := s.getRangeIDLocked()
	request.RangeID = currentRangeID
	resp, err := s.executionManager.UpdateWorkflowExecution(request)
	if err = s.handleErrorLocked(err); err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *ContextImpl) ConflictResolveWorkflowExecution(
	request *persistence.ConflictResolveWorkflowExecutionRequest,
) (*persistence.ConflictResolveWorkflowExecutionResponse, error) {
	if err := s.errorByState(); err != nil {
		return nil, err
	}

	namespaceID := namespace.ID(request.ResetWorkflowSnapshot.ExecutionInfo.NamespaceId)
	workflowID := request.ResetWorkflowSnapshot.ExecutionInfo.WorkflowId

	// do not try to get namespace cache within shard lock
	namespaceEntry, err := s.GetNamespaceRegistry().GetNamespaceByID(namespaceID)
	if err != nil {
		return nil, err
	}

	s.wLock()
	defer s.wUnlock()

	transferMaxReadLevel := int64(0)
	if request.CurrentWorkflowMutation != nil {
		if err := s.allocateTaskIDsLocked(
			namespaceEntry,
			workflowID,
			request.CurrentWorkflowMutation.TransferTasks,
			request.CurrentWorkflowMutation.ReplicationTasks,
			request.CurrentWorkflowMutation.TimerTasks,
			request.CurrentWorkflowMutation.VisibilityTasks,
			&transferMaxReadLevel,
		); err != nil {
			return nil, err
		}
	}
	if err := s.allocateTaskIDsLocked(
		namespaceEntry,
		workflowID,
		request.ResetWorkflowSnapshot.TransferTasks,
		request.ResetWorkflowSnapshot.ReplicationTasks,
		request.ResetWorkflowSnapshot.TimerTasks,
		request.ResetWorkflowSnapshot.VisibilityTasks,
		&transferMaxReadLevel,
	); err != nil {
		return nil, err
	}
	if request.NewWorkflowSnapshot != nil {
		if err := s.allocateTaskIDsLocked(
			namespaceEntry,
			workflowID,
			request.NewWorkflowSnapshot.TransferTasks,
			request.NewWorkflowSnapshot.ReplicationTasks,
			request.NewWorkflowSnapshot.TimerTasks,
			request.NewWorkflowSnapshot.VisibilityTasks,
			&transferMaxReadLevel,
		); err != nil {
			return nil, err
		}
	}
	defer s.updateMaxReadLevelLocked(transferMaxReadLevel)

	currentRangeID := s.getRangeIDLocked()
	request.RangeID = currentRangeID
	resp, err := s.executionManager.ConflictResolveWorkflowExecution(request)
	if err := s.handleErrorLocked(err); err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *ContextImpl) AddTasks(
	request *persistence.AddTasksRequest,
) error {
	if err := s.errorByState(); err != nil {
		return err
	}

	namespaceID := namespace.ID(request.NamespaceID)

	// do not try to get namespace cache within shard lock
	namespaceEntry, err := s.GetNamespaceRegistry().GetNamespaceByID(namespaceID)
	if err != nil {
		return err
	}

	s.wLock()
	defer s.wUnlock()

	return s.addTasksLocked(request, namespaceEntry)
}

func (s *ContextImpl) addTasksLocked(
	request *persistence.AddTasksRequest,
	namespaceEntry *namespace.Namespace,
) error {
	transferMaxReadLevel := int64(0)
	if err := s.allocateTaskIDsLocked(
		namespaceEntry,
		request.WorkflowID,
		request.TransferTasks,
		request.ReplicationTasks,
		request.TimerTasks,
		request.VisibilityTasks,
		&transferMaxReadLevel,
	); err != nil {
		return err
	}
	defer s.updateMaxReadLevelLocked(transferMaxReadLevel)

	request.RangeID = s.getRangeIDLocked()
	err := s.executionManager.AddTasks(request)
	if err = s.handleErrorLocked(err); err != nil {
		return err
	}
	s.engine.NotifyNewTransferTasks(request.TransferTasks)
	s.engine.NotifyNewTimerTasks(request.TimerTasks)
	s.engine.NotifyNewVisibilityTasks(request.VisibilityTasks)
	s.engine.NotifyNewReplicationTasks(request.ReplicationTasks)
	return nil
}

func (s *ContextImpl) AppendHistoryEvents(
	request *persistence.AppendHistoryNodesRequest,
	namespaceID namespace.ID,
	execution commonpb.WorkflowExecution,
) (int, error) {
	if err := s.errorByState(); err != nil {
		return 0, err
	}

	request.ShardID = s.shardID

	size := 0
	defer func() {
		// N.B. - Dual emit here makes sense so that we can see aggregate timer stats across all
		// namespaces along with the individual namespaces stats
		s.GetMetricsClient().RecordDistribution(metrics.SessionSizeStatsScope, metrics.HistorySize, size)
		if entry, err := s.GetNamespaceRegistry().GetNamespaceByID(namespaceID); err == nil && entry != nil {
			s.GetMetricsClient().Scope(
				metrics.SessionSizeStatsScope,
				metrics.NamespaceTag(entry.Name().String()),
			).RecordDistribution(metrics.HistorySize, size)
		}
		if size >= historySizeLogThreshold {
			s.throttledLogger.Warn("history size threshold breached",
				tag.WorkflowID(execution.GetWorkflowId()),
				tag.WorkflowRunID(execution.GetRunId()),
				tag.WorkflowNamespaceID(namespaceID.String()),
				tag.WorkflowHistorySizeBytes(size))
		}
	}()
	resp, err0 := s.GetExecutionManager().AppendHistoryNodes(request)
	if resp != nil {
		size = resp.Size
	}
	return size, err0
}

func (s *ContextImpl) DeleteWorkflowExecution(
	key definition.WorkflowKey,
	branchToken []byte,
	version int64,
) error {
	if err := s.errorByState(); err != nil {
		return err
	}

	// do not try to get namespace cache within shard lock
	namespaceEntry, err := s.GetNamespaceRegistry().GetNamespaceByID(namespace.ID(key.NamespaceID))
	if err != nil {
		return err
	}

	s.wLock()
	defer s.wUnlock()

	delCurRequest := &persistence.DeleteCurrentWorkflowExecutionRequest{
		ShardID:     s.shardID,
		NamespaceID: key.NamespaceID,
		WorkflowID:  key.WorkflowID,
		RunID:       key.RunID,
	}
	op := func() error {
		return s.GetExecutionManager().DeleteCurrentWorkflowExecution(delCurRequest)
	}
	err = backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
	if err != nil {
		return err
	}

	delRequest := &persistence.DeleteWorkflowExecutionRequest{
		ShardID:     s.shardID,
		NamespaceID: key.NamespaceID,
		WorkflowID:  key.WorkflowID,
		RunID:       key.RunID,
	}
	op = func() error {
		return s.GetExecutionManager().DeleteWorkflowExecution(delRequest)
	}
	err = backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
	if err != nil {
		return err
	}

	if branchToken != nil {
		delHistoryRequest := &persistence.DeleteHistoryBranchRequest{
			BranchToken: branchToken,
			ShardID:     s.shardID,
		}
		op := func() error {
			return s.GetExecutionManager().DeleteHistoryBranch(delHistoryRequest)
		}
		err = backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
		if err != nil {
			return err
		}
	}

	// Delete visibility
	addTasksRequest := &persistence.AddTasksRequest{
		ShardID:     s.shardID,
		NamespaceID: key.NamespaceID,
		WorkflowID:  key.WorkflowID,
		RunID:       key.RunID,

		TransferTasks:    nil,
		TimerTasks:       nil,
		ReplicationTasks: nil,
		VisibilityTasks: []tasks.Task{&tasks.DeleteExecutionVisibilityTask{
			// TaskID is set by addTasksLocked
			WorkflowKey:         key,
			VisibilityTimestamp: s.GetTimeSource().Now(),
			Version:             version,
		}},
	}
	err = s.addTasksLocked(addTasksRequest, namespaceEntry)
	if err != nil {
		return err
	}

	return nil
}

func (s *ContextImpl) GetConfig() *configs.Config {
	// constant from initialization, no need for locks
	return s.config
}

func (s *ContextImpl) GetEventsCache() events.Cache {
	// constant from initialization (except for tests), no need for locks
	return s.eventsCache
}

func (s *ContextImpl) GetLogger() log.Logger {
	// constant from initialization, no need for locks
	return s.logger
}

func (s *ContextImpl) GetThrottledLogger() log.Logger {
	// constant from initialization, no need for locks
	return s.throttledLogger
}

func (s *ContextImpl) getRangeIDLocked() int64 {
	return s.shardInfo.GetRangeId()
}

func (s *ContextImpl) errorByState() error {
	s.rLock()
	defer s.rUnlock()
	return s.errorByStateLocked()
}

func (s *ContextImpl) errorByStateLocked() error {
	switch s.state {
	case contextStateInitialized, contextStateAcquiring:
		return ErrShardStatusUnknown
	case contextStateAcquired:
		return nil
	case contextStateStopping, contextStateStopped:
		return ErrShardClosed
	default:
		panic("invalid state")
	}
}

func (s *ContextImpl) generateTransferTaskIDLocked() (int64, error) {
	if err := s.updateRangeIfNeededLocked(); err != nil {
		return -1, err
	}

	taskID := s.transferSequenceNumber
	s.transferSequenceNumber++

	return taskID, nil
}

func (s *ContextImpl) updateRangeIfNeededLocked() error {
	if s.transferSequenceNumber < s.maxTransferSequenceNumber {
		return nil
	}

	return s.renewRangeLocked(false)
}

func (s *ContextImpl) renewRangeLocked(isStealing bool) error {
	updatedShardInfo := copyShardInfo(s.shardInfo)
	updatedShardInfo.RangeId++
	if isStealing {
		updatedShardInfo.StolenSinceRenew++
	}

	err := s.GetShardManager().UpdateShard(&persistence.UpdateShardRequest{
		ShardInfo:       updatedShardInfo.ShardInfo,
		PreviousRangeID: s.shardInfo.GetRangeId()})
	if err != nil {
		// Failure in updating shard to grab new RangeID
		s.logger.Error("Persistent store operation failure",
			tag.StoreOperationUpdateShard,
			tag.Error(err),
			tag.ShardRangeID(updatedShardInfo.GetRangeId()),
			tag.PreviousShardRangeID(s.shardInfo.GetRangeId()),
		)
		return s.handleErrorLocked(err)
	}

	// Range is successfully updated in cassandra now update shard context to reflect new range
	s.logger.Info("Range updated for shardID",
		tag.ShardRangeID(updatedShardInfo.RangeId),
		tag.PreviousShardRangeID(s.shardInfo.RangeId),
		tag.Number(s.transferSequenceNumber),
		tag.NextNumber(s.maxTransferSequenceNumber),
	)

	s.transferSequenceNumber = updatedShardInfo.GetRangeId() << s.config.RangeSizeBits
	s.maxTransferSequenceNumber = (updatedShardInfo.GetRangeId() + 1) << s.config.RangeSizeBits
	s.transferMaxReadLevel = s.transferSequenceNumber - 1
	s.shardInfo = updatedShardInfo

	return nil
}

func (s *ContextImpl) updateMaxReadLevelLocked(rl int64) {
	if rl > s.transferMaxReadLevel {
		s.logger.Debug("Updating MaxTaskID", tag.MaxLevel(rl))
		s.transferMaxReadLevel = rl
	}
}

func (s *ContextImpl) updateShardInfoLocked() error {
	if err := s.errorByStateLocked(); err != nil {
		return err
	}

	var err error
	now := clock.NewRealTimeSource().Now()
	if s.lastUpdated.Add(s.config.ShardUpdateMinInterval()).After(now) {
		return nil
	}
	updatedShardInfo := copyShardInfo(s.shardInfo)
	s.emitShardInfoMetricsLogsLocked()

	err = s.GetShardManager().UpdateShard(&persistence.UpdateShardRequest{
		ShardInfo:       updatedShardInfo.ShardInfo,
		PreviousRangeID: s.shardInfo.GetRangeId(),
	})
	if err != nil {
		return s.handleErrorLocked(err)
	}

	s.lastUpdated = now
	return nil
}

func (s *ContextImpl) emitShardInfoMetricsLogsLocked() {
	currentCluster := s.GetClusterMetadata().GetCurrentClusterName()

	minTransferLevel := s.shardInfo.ClusterTransferAckLevel[currentCluster]
	maxTransferLevel := s.shardInfo.ClusterTransferAckLevel[currentCluster]
	for _, v := range s.shardInfo.ClusterTransferAckLevel {
		if v < minTransferLevel {
			minTransferLevel = v
		}
		if v > maxTransferLevel {
			maxTransferLevel = v
		}
	}
	diffTransferLevel := maxTransferLevel - minTransferLevel

	minTimerLevel := timestamp.TimeValue(s.shardInfo.ClusterTimerAckLevel[currentCluster])
	maxTimerLevel := timestamp.TimeValue(s.shardInfo.ClusterTimerAckLevel[currentCluster])
	for _, v := range s.shardInfo.ClusterTimerAckLevel {
		t := timestamp.TimeValue(v)
		if t.Before(minTimerLevel) {
			minTimerLevel = t
		}
		if t.After(maxTimerLevel) {
			maxTimerLevel = t
		}
	}
	diffTimerLevel := maxTimerLevel.Sub(minTimerLevel)

	replicationLag := s.transferMaxReadLevel - s.shardInfo.ReplicationAckLevel
	transferLag := s.transferMaxReadLevel - s.shardInfo.TransferAckLevel
	timerLag := time.Since(timestamp.TimeValue(s.shardInfo.TimerAckLevelTime))

	transferFailoverInProgress := len(s.shardInfo.TransferFailoverLevels)
	timerFailoverInProgress := len(s.shardInfo.TimerFailoverLevels)

	if s.config.EmitShardDiffLog() &&
		(logWarnTransferLevelDiff < diffTransferLevel ||
			logWarnTimerLevelDiff < diffTimerLevel ||
			logWarnTransferLevelDiff < transferLag ||
			logWarnTimerLevelDiff < timerLag) {

		s.logger.Warn("Shard ack levels diff exceeds warn threshold.",
			tag.ShardReplicationAck(s.shardInfo.ReplicationAckLevel),
			tag.ShardTimerAcks(s.shardInfo.ClusterTimerAckLevel),
			tag.ShardTransferAcks(s.shardInfo.ClusterTransferAckLevel))
	}

	s.GetMetricsClient().RecordDistribution(metrics.ShardInfoScope, metrics.ShardInfoTransferDiffTimer, int(diffTransferLevel))
	s.GetMetricsClient().RecordTimer(metrics.ShardInfoScope, metrics.ShardInfoTimerDiffTimer, diffTimerLevel)

	s.GetMetricsClient().RecordDistribution(metrics.ShardInfoScope, metrics.ShardInfoReplicationLagTimer, int(replicationLag))
	s.GetMetricsClient().RecordDistribution(metrics.ShardInfoScope, metrics.ShardInfoTransferLagTimer, int(transferLag))
	s.GetMetricsClient().RecordTimer(metrics.ShardInfoScope, metrics.ShardInfoTimerLagTimer, timerLag)

	s.GetMetricsClient().RecordDistribution(metrics.ShardInfoScope, metrics.ShardInfoTransferFailoverInProgressTimer, transferFailoverInProgress)
	s.GetMetricsClient().RecordDistribution(metrics.ShardInfoScope, metrics.ShardInfoTimerFailoverInProgressTimer, timerFailoverInProgress)
}

func (s *ContextImpl) allocateTaskIDsLocked(
	namespaceEntry *namespace.Namespace,
	workflowID string,
	transferTasks []tasks.Task,
	replicationTasks []tasks.Task,
	timerTasks []tasks.Task,
	visibilityTasks []tasks.Task,
	transferMaxReadLevel *int64,
) error {

	if err := s.allocateTransferIDsLocked(
		transferTasks,
		transferMaxReadLevel); err != nil {
		return err
	}
	if err := s.allocateTransferIDsLocked(
		replicationTasks,
		transferMaxReadLevel); err != nil {
		return err
	}
	if err := s.allocateTransferIDsLocked(
		visibilityTasks,
		transferMaxReadLevel); err != nil {
		return err
	}
	return s.allocateTimerIDsLocked(
		namespaceEntry,
		workflowID,
		timerTasks)
}

func (s *ContextImpl) allocateTransferIDsLocked(
	tasks []tasks.Task,
	transferMaxReadLevel *int64,
) error {

	for _, task := range tasks {
		id, err := s.generateTransferTaskIDLocked()
		if err != nil {
			return err
		}
		s.logger.Debug("Assigning task ID", tag.TaskID(id))
		task.SetTaskID(id)
		*transferMaxReadLevel = id
	}
	return nil
}

// NOTE: allocateTimerIDsLocked should always been called after assigning taskID for transferTasks when assigning taskID together,
// because Temporal Indexer assume timer taskID of deleteWorkflowExecution is larger than transfer taskID of closeWorkflowExecution
// for a given workflow.
func (s *ContextImpl) allocateTimerIDsLocked(
	namespaceEntry *namespace.Namespace,
	workflowID string,
	timerTasks []tasks.Task,
) error {

	// assign IDs for the timer tasks. They need to be assigned under shard lock.
	currentCluster := s.GetClusterMetadata().GetCurrentClusterName()
	for _, task := range timerTasks {
		ts := task.GetVisibilityTime()
		if task.GetVersion() != common.EmptyVersion {
			// cannot use version to determine the corresponding cluster for timer task
			// this is because during failover, timer task should be created as active
			// or otherwise, failover + active processing logic may not pick up the task.
			currentCluster = namespaceEntry.ActiveClusterName()
		}
		readCursorTS := s.timerMaxReadLevelMap[currentCluster]
		if ts.Before(readCursorTS) {
			// This can happen if shard move and new host have a time SKU, or there is db write delay.
			// We generate a new timer ID using timerMaxReadLevel.
			s.logger.Debug("New timer generated is less than read level",
				tag.WorkflowNamespaceID(namespaceEntry.ID().String()),
				tag.WorkflowID(workflowID),
				tag.Timestamp(ts),
				tag.CursorTimestamp(readCursorTS),
				tag.ValueShardAllocateTimerBeforeRead)
			task.SetVisibilityTime(s.timerMaxReadLevelMap[currentCluster].Add(time.Millisecond))
		}

		seqNum, err := s.generateTransferTaskIDLocked()
		if err != nil {
			return err
		}
		task.SetTaskID(seqNum)
		visibilityTs := task.GetVisibilityTime()
		s.logger.Debug("Assigning new timer",
			tag.Timestamp(visibilityTs), tag.TaskID(task.GetTaskID()), tag.AckLevel(s.shardInfo.TimerAckLevelTime))
	}
	return nil
}

func (s *ContextImpl) SetCurrentTime(cluster string, currentTime time.Time) {
	s.wLock()
	defer s.wUnlock()
	if cluster != s.GetClusterMetadata().GetCurrentClusterName() {
		prevTime := s.getRemoteClusterInfoLocked(cluster).CurrentTime
		if prevTime.Before(currentTime) {
			s.getRemoteClusterInfoLocked(cluster).CurrentTime = currentTime
		}
	} else {
		panic("Cannot set current time for current cluster")
	}
}

func (s *ContextImpl) GetCurrentTime(cluster string) time.Time {
	s.rLock()
	defer s.rUnlock()
	if cluster != s.GetClusterMetadata().GetCurrentClusterName() {
		return s.getRemoteClusterInfoLocked(cluster).CurrentTime
	}
	return s.GetTimeSource().Now().UTC()
}

func (s *ContextImpl) GetLastUpdatedTime() time.Time {
	s.rLock()
	defer s.rUnlock()
	return s.lastUpdated
}

func (s *ContextImpl) handleErrorLocked(err error) error {
	switch err.(type) {
	case nil:
		return nil

	case *persistence.CurrentWorkflowConditionFailedError,
		*persistence.WorkflowConditionFailedError,
		*persistence.ConditionFailedError,
		*serviceerror.ResourceExhausted:
		// No special handling required for these errors
		return err

	case *persistence.ShardOwnershipLostError:
		// Shard is stolen, trigger shutdown of history engine
		s.transitionLocked(contextRequestStop)
		return err

	default:
		// We have no idea if the write failed or will eventually make it to persistence. Try to re-acquire
		// the shard in the background. If successful, we'll get a new RangeID, to guarantee that subsequent
		// reads will either see that write, or know for certain that it failed. This allows the callers to
		// reliably check the outcome by performing a read. If we fail, we'll shut down the shard.
		s.transitionLocked(contextRequestLost)
		return err
	}
}

func (s *ContextImpl) maybeRecordShardAcquisitionLatency(ownershipChanged bool) {
	if ownershipChanged {
		s.GetMetricsClient().RecordTimer(metrics.ShardInfoScope, metrics.ShardContextAcquisitionLatency,
			s.GetCurrentTime(s.GetClusterMetadata().GetCurrentClusterName()).Sub(s.GetLastUpdatedTime()))
	}
}

func (s *ContextImpl) createEngine() Engine {
	s.logger.Info("", tag.LifeCycleStarting, tag.ComponentShardEngine)
	engine := s.engineFactory.CreateEngine(s)
	engine.Start()
	s.logger.Info("", tag.LifeCycleStarted, tag.ComponentShardEngine)
	return engine
}

func (s *ContextImpl) getOrCreateEngine(ctx context.Context) (engine Engine, retErr error) {
	// Block on shard acquisition for the lifetime of this context. Note that this retry is just
	// polling a value in memory. Another goroutine is doing the actual work.
	policy := backoff.NewExponentialRetryPolicy(5 * time.Millisecond)
	policy.SetMaximumInterval(1 * time.Second)

	isRetryable := func(err error) bool { return err == ErrShardStatusUnknown }

	op := func(context.Context) error {
		s.rLock()
		defer s.rUnlock()
		err := s.errorByStateLocked()
		if err == nil {
			engine = s.engine
		}
		return err
	}

	retErr = backoff.RetryContext(ctx, op, policy, isRetryable)
	if retErr == nil && engine == nil {
		// This shouldn't ever happen, but don't let it return nil error.
		retErr = ErrShardStatusUnknown
	}
	return
}

// start should only be called by the controller.
func (s *ContextImpl) start() {
	s.wLock()
	defer s.wUnlock()
	s.transitionLocked(contextRequestAcquire)
}

// stop should only be called by the controller.
func (s *ContextImpl) stop() {
	s.wLock()
	s.transitionLocked(contextRequestFinishStop)
	engine := s.engine
	s.engine = nil
	s.wUnlock()

	// Stop the engine if it was running (outside the lock but before returning)
	if engine != nil {
		s.logger.Info("", tag.LifeCycleStopping, tag.ComponentShardEngine)
		engine.Stop()
		s.logger.Info("", tag.LifeCycleStopped, tag.ComponentShardEngine)
	}
}

func (s *ContextImpl) isValid() bool {
	s.rLock()
	defer s.rUnlock()
	return s.state < contextStateStopping
}

func (s *ContextImpl) wLock() {
	scope := metrics.ShardInfoScope
	s.metricsClient.IncCounter(scope, metrics.LockRequests)
	sw := s.metricsClient.StartTimer(scope, metrics.LockLatency)
	defer sw.Stop()

	s.rwLock.Lock()
}

func (s *ContextImpl) rLock() {
	scope := metrics.ShardInfoScope
	s.metricsClient.IncCounter(scope, metrics.LockRequests)
	sw := s.metricsClient.StartTimer(scope, metrics.LockLatency)
	defer sw.Stop()

	s.rwLock.RLock()
}

func (s *ContextImpl) wUnlock() {
	s.rwLock.Unlock()
}

func (s *ContextImpl) rUnlock() {
	s.rwLock.RUnlock()
}

func (s *ContextImpl) transitionLocked(request contextRequest) {
	/* State transitions:

	The normal pattern:
		Initialized
			controller calls start()
		Acquiring
			acquireShard gets the shard
		Acquired

	If we get a transient error from persistence:
		Acquired
			transient error: handleErrorLocked calls transitionLocked(contextRequestLost)
		Acquiring
			acquireShard gets the shard
		Acquired

	If we get shard ownership lost:
		Acquired
			ShardOwnershipLostError: handleErrorLocked calls transitionLocked(contextRequestStop)
		Stopping
			controller removes from map and calls stop()
		Stopped

	Stopping can be triggered internally (if we get a ShardOwnershipLostError, or fail to acquire the rangeid
	lock after several minutes) or externally (from controller, e.g. controller shutting down or admin force-
	unload shard). If it's triggered internally, we transition to Stopping, then make an asynchronous callback
	to controller, which will remove us from the map and call stop(), which will transition to Stopped and
	stop the engine. If it's triggered externally, we'll skip over Stopping and go straight to Stopped.

	If we want to stop, and the acquireShard goroutine is still running, we can't kill it, but we need a
	mechanism to make sure it doesn't make any persistence calls or state transitions. We make acquireShard
	check the state each time it acquires the lock, and do nothing if the state has changed to Stopping (or
	Stopped).

	Invariants:
	- Once state is Stopping, it can only go to Stopped.
	- Once state is Stopped, it can't go anywhere else.
	- At the start of acquireShard, state must be Acquiring.
	- By the end of acquireShard, state must not be Acquiring: either acquireShard set it to Acquired, or the
	  controller set it to Stopped.
	- If state is Acquiring, acquireShard should be running in the background.
	- Only acquireShard can use contextRequestAcquired (i.e. transition from Acquiring to Acquired).
	- Once state has reached Acquired at least once, and not reached Stopped, engine must be non-nil.
	- Only the controller may call start() and stop().
	- The controller must call stop() for every ContextImpl it creates.

	*/

	setStateAcquiring := func() {
		s.state = contextStateAcquiring
		go s.acquireShard()
	}

	setStateStopping := func() {
		s.state = contextStateStopping
		// The change in state should cause all write methods to fail, but just in case, set this also,
		// which will cause failures at the persistence level. (Note that if persistence is unavailable
		// and we couldn't even load the shard metadata, shardInfo may still be nil here.)
		if s.shardInfo != nil {
			s.shardInfo.RangeId = -1
		}
		// This will cause the controller to remove this shard from the map and then call s.stop()
		go s.closeCallback(s)
	}

	setStateStopped := func() {
		s.state = contextStateStopped
	}

	switch s.state {
	case contextStateInitialized:
		switch request {
		case contextRequestAcquire:
			setStateAcquiring()
			return
		case contextRequestStop:
			setStateStopping()
			return
		case contextRequestFinishStop:
			setStateStopped()
			return
		}
	case contextStateAcquiring:
		switch request {
		case contextRequestAcquire:
			return // nothing to do, already acquiring
		case contextRequestAcquired:
			s.state = contextStateAcquired
			return
		case contextRequestLost:
			return // nothing to do, already acquiring
		case contextRequestStop:
			setStateStopping()
			return
		case contextRequestFinishStop:
			setStateStopped()
			return
		}
	case contextStateAcquired:
		switch request {
		case contextRequestAcquire:
			return // nothing to to do, already acquired
		case contextRequestLost:
			setStateAcquiring()
			return
		case contextRequestStop:
			setStateStopping()
			return
		case contextRequestFinishStop:
			setStateStopped()
			return
		}
	case contextStateStopping:
		switch request {
		case contextRequestStop:
			// nothing to do, already stopping
			return
		case contextRequestFinishStop:
			setStateStopped()
			return
		}
	}
	s.logger.Warn("invalid state transition request",
		tag.ShardContextState(int(s.state)),
		tag.ShardContextStateRequest(int(request)),
	)
}

func (s *ContextImpl) loadShardMetadata(ownershipChanged *bool) error {
	// Only have to do this once, we can just re-acquire the rangeid lock after that
	s.rLock()

	if s.state >= contextStateStopping {
		return errStoppingContext
	}

	if s.shardInfo != nil {
		s.rUnlock()
		return nil
	}

	s.rUnlock()

	// We don't have any shardInfo yet, load it (outside of context rwlock)
	resp, err := s.GetShardManager().GetOrCreateShard(&persistence.GetOrCreateShardRequest{
		ShardID:         s.shardID,
		CreateIfMissing: true,
	})
	if err != nil {
		s.logger.Error("Failed to load shard", tag.Error(err))
		return err
	}
	shardInfo := &persistence.ShardInfoWithFailover{ShardInfo: resp.ShardInfo}

	// shardInfo is a fresh value, so we don't really need to copy, but
	// copyShardInfo also ensures that all maps are non-nil
	updatedShardInfo := copyShardInfo(shardInfo)
	*ownershipChanged = shardInfo.Owner != s.GetHostInfo().Identity()
	updatedShardInfo.Owner = s.GetHostInfo().Identity()

	// initialize the cluster current time to be the same as ack level
	remoteClusterInfos := make(map[string]*remoteClusterInfo)
	timerMaxReadLevelMap := make(map[string]time.Time)
	for clusterName, info := range s.GetClusterMetadata().GetAllClusterInfo() {
		if !info.Enabled {
			continue
		}

		currentReadTime := timestamp.TimeValue(shardInfo.TimerAckLevelTime)
		if clusterName != s.GetClusterMetadata().GetCurrentClusterName() {
			if currentTime, ok := shardInfo.ClusterTimerAckLevel[clusterName]; ok {
				currentReadTime = timestamp.TimeValue(currentTime)
			}

			remoteClusterInfos[clusterName] = &remoteClusterInfo{CurrentTime: currentReadTime}
			timerMaxReadLevelMap[clusterName] = currentReadTime
		} else { // active cluster
			timerMaxReadLevelMap[clusterName] = currentReadTime
		}

		timerMaxReadLevelMap[clusterName] = timerMaxReadLevelMap[clusterName].Truncate(time.Millisecond)
	}

	s.wLock()
	defer s.wUnlock()

	if s.state >= contextStateStopping {
		return errStoppingContext
	}

	s.shardInfo = updatedShardInfo
	s.remoteClusterInfos = remoteClusterInfos
	s.timerMaxReadLevelMap = timerMaxReadLevelMap

	return nil
}

func (s *ContextImpl) GetRemoteClusterAckInfo(cluster []string) (map[string]*historyservice.ShardReplicationStatusPerCluster, error) {
	resp := make(map[string]*historyservice.ShardReplicationStatusPerCluster)
	s.rLock()
	defer s.rUnlock()
	if len(cluster) == 0 {
		// remote acked info for all known remote clusters
		for k, v := range s.remoteClusterInfos {
			resp[k] = &historyservice.ShardReplicationStatusPerCluster{
				AckedTaskId:             v.AckedReplicationTaskID,
				AckedTaskVisibilityTime: timestamp.TimePtr(v.AckedReplicationTimestamp),
			}
		}
	} else {
		for _, k := range cluster {
			if v, ok := s.remoteClusterInfos[k]; ok {
				resp[k] = &historyservice.ShardReplicationStatusPerCluster{
					AckedTaskId:             v.AckedReplicationTaskID,
					AckedTaskVisibilityTime: timestamp.TimePtr(v.AckedReplicationTimestamp),
				}
			}
		}
	}

	return resp, nil
}

func (s *ContextImpl) getRemoteClusterInfoLocked(clusterName string) *remoteClusterInfo {
	if info, ok := s.remoteClusterInfos[clusterName]; ok {
		return info
	}
	info := &remoteClusterInfo{
		AckedReplicationTaskID: persistence.EmptyQueueMessageID,
	}
	s.remoteClusterInfos[clusterName] = info
	return info
}

func (s *ContextImpl) acquireShard() {
	// Retry for 5m, with interval up to 10s (default)
	policy := backoff.NewExponentialRetryPolicy(50 * time.Millisecond)
	policy.SetExpirationInterval(5 * time.Minute)

	// Remember this value across attempts
	ownershipChanged := false

	op := func() error {
		// Initial load of shard metadata
		err := s.loadShardMetadata(&ownershipChanged)
		if err != nil {
			return err
		}

		s.wLock()
		defer s.wUnlock()

		// Check that we should still be running
		if s.state >= contextStateStopping {
			return errStoppingContext
		}

		// Try to acquire RangeID lock. If this gets a persistence error, it may call:
		// transitionLocked(contextRequestStop) for ShardOwnershipLostError:
		//   This will transition to Stopping right here, and the transitionLocked call at the end of the
		//   outer function will do nothing, since the state was already changed.
		// transitionLocked(contextRequestLost) for other transient errors:
		//   This will do nothing, since state is already Acquiring.
		err = s.renewRangeLocked(true)
		if err != nil {
			return err
		}

		s.logger.Info("Acquired shard")

		// The first time we get the shard, we have to create the engine. We have to release the lock to
		// create the engine, and then reacquire it. This is safe because:
		// 1. We know we're currently in the Acquiring state. The only thing we can transition to (without
		//    doing it ourselves) is Stopped. In that case, we'll have to stop the engine that we just
		//    created, since the stop transition didn't do it.
		// 2. We don't have an engine yet, so no one should be calling any of our methods that mutate things.
		if s.engine == nil {
			s.wUnlock()
			s.maybeRecordShardAcquisitionLatency(ownershipChanged)
			engine := s.createEngine()
			s.wLock()
			if s.state >= contextStateStopping {
				engine.Stop()
				return errStoppingContext
			}
			s.engine = engine
		}
		s.transitionLocked(contextRequestAcquired)
		return nil
	}

	err := backoff.Retry(op, policy, common.IsPersistenceTransientError)
	if err == errStoppingContext {
		// State changed since this goroutine started, exit silently.
		return
	} else if err != nil {
		// We got an unretryable error (perhaps ShardOwnershipLostError) or timed out.
		s.logger.Error("Couldn't acquire shard", tag.Error(err))

		// If there's been another state change since we started (e.g. to Stopping), then don't do anything
		// here. But if not (i.e. timed out or error), initiate shutting down the shard.
		s.wLock()
		defer s.wUnlock()
		if s.state >= contextStateStopping {
			return
		}
		s.transitionLocked(contextRequestStop)
	}
}

func newContext(
	resource resource.Resource,
	shardID int32,
	factory EngineFactory,
	config *configs.Config,
	closeCallback func(*ContextImpl),
) (*ContextImpl, error) {

	hostIdentity := resource.GetHostInfo().Identity()

	shardContext := &ContextImpl{
		Resource:         resource,
		state:            contextStateInitialized,
		shardID:          shardID,
		executionManager: resource.GetExecutionManager(),
		metricsClient:    resource.GetMetricsClient(),
		closeCallback:    closeCallback,
		config:           config,
		logger:           log.With(resource.GetLogger(), tag.ShardID(shardID), tag.Address(hostIdentity)),
		throttledLogger:  log.With(resource.GetThrottledLogger(), tag.ShardID(shardID), tag.Address(hostIdentity)),
		engineFactory:    factory,
	}
	shardContext.eventsCache = events.NewEventsCache(
		shardContext.GetShardID(),
		shardContext.GetConfig().EventsCacheInitialSize(),
		shardContext.GetConfig().EventsCacheMaxSize(),
		shardContext.GetConfig().EventsCacheTTL(),
		shardContext.GetExecutionManager(),
		false,
		shardContext.GetLogger(),
		shardContext.GetMetricsClient(),
	)

	return shardContext, nil
}

func copyShardInfo(shardInfo *persistence.ShardInfoWithFailover) *persistence.ShardInfoWithFailover {
	transferFailoverLevels := map[string]persistence.TransferFailoverLevel{}
	for k, v := range shardInfo.TransferFailoverLevels {
		transferFailoverLevels[k] = v
	}
	timerFailoverLevels := map[string]persistence.TimerFailoverLevel{}
	for k, v := range shardInfo.TimerFailoverLevels {
		timerFailoverLevels[k] = v
	}
	clusterTransferAckLevel := make(map[string]int64)
	for k, v := range shardInfo.ClusterTransferAckLevel {
		clusterTransferAckLevel[k] = v
	}
	clusterTimerAckLevel := make(map[string]*time.Time)
	for k, v := range shardInfo.ClusterTimerAckLevel {
		if timestamp.TimeValue(v).IsZero() {
			v = timestamp.TimePtr(defaultTime)
		}
		clusterTimerAckLevel[k] = v
	}
	clusterReplicationLevel := make(map[string]int64)
	for k, v := range shardInfo.ClusterReplicationLevel {
		clusterReplicationLevel[k] = v
	}
	clusterReplicationDLQLevel := make(map[string]int64)
	for k, v := range shardInfo.ReplicationDlqAckLevel {
		clusterReplicationDLQLevel[k] = v
	}
	if timestamp.TimeValue(shardInfo.TimerAckLevelTime).IsZero() {
		shardInfo.TimerAckLevelTime = timestamp.TimePtr(defaultTime)
	}
	shardInfoCopy := &persistence.ShardInfoWithFailover{
		ShardInfo: &persistencespb.ShardInfo{
			ShardId:                      shardInfo.GetShardId(),
			Owner:                        shardInfo.Owner,
			RangeId:                      shardInfo.GetRangeId(),
			StolenSinceRenew:             shardInfo.StolenSinceRenew,
			ReplicationAckLevel:          shardInfo.ReplicationAckLevel,
			TransferAckLevel:             shardInfo.TransferAckLevel,
			TimerAckLevelTime:            shardInfo.TimerAckLevelTime,
			ClusterTransferAckLevel:      clusterTransferAckLevel,
			ClusterTimerAckLevel:         clusterTimerAckLevel,
			NamespaceNotificationVersion: shardInfo.NamespaceNotificationVersion,
			ClusterReplicationLevel:      clusterReplicationLevel,
			ReplicationDlqAckLevel:       clusterReplicationDLQLevel,
			UpdateTime:                   shardInfo.UpdateTime,
			VisibilityAckLevel:           shardInfo.VisibilityAckLevel,
		},
		TransferFailoverLevels: transferFailoverLevels,
		TimerFailoverLevels:    timerFailoverLevels,
	}

	return shardInfoCopy
}
