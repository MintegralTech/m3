// Copyright (c) 2016 Uber Technologies, Inc.
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

package bootstrap

import (
	"sync"
	"time"

	"github.com/m3db/m3/src/dbnode/clock"
	"github.com/m3db/m3/src/dbnode/namespace"
	"github.com/m3db/m3/src/dbnode/persist"
	"github.com/m3db/m3/src/dbnode/retention"
	"github.com/m3db/m3/src/dbnode/sharding"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap/result"
	"github.com/m3db/m3/src/dbnode/topology"
	xtime "github.com/m3db/m3/src/x/time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// bootstrapProcessProvider is the bootstrapping process provider.
type bootstrapProcessProvider struct {
	sync.RWMutex
	processOpts          ProcessOptions
	resultOpts           result.Options
	log                  *zap.Logger
	bootstrapperProvider BootstrapperProvider
}

type bootstrapRunType string

const (
	bootstrapDataRunType  = bootstrapRunType("bootstrap-data")
	bootstrapIndexRunType = bootstrapRunType("bootstrap-index")
)

// NewProcessProvider creates a new bootstrap process provider.
func NewProcessProvider(
	bootstrapperProvider BootstrapperProvider,
	processOpts ProcessOptions,
	resultOpts result.Options,
) (ProcessProvider, error) {
	if err := processOpts.Validate(); err != nil {
		return nil, err
	}

	return &bootstrapProcessProvider{
		processOpts:          processOpts,
		resultOpts:           resultOpts,
		log:                  resultOpts.InstrumentOptions().Logger(),
		bootstrapperProvider: bootstrapperProvider,
	}, nil
}

func (b *bootstrapProcessProvider) SetBootstrapperProvider(bootstrapperProvider BootstrapperProvider) {
	b.Lock()
	defer b.Unlock()
	b.bootstrapperProvider = bootstrapperProvider
}

func (b *bootstrapProcessProvider) BootstrapperProvider() BootstrapperProvider {
	b.RLock()
	defer b.RUnlock()
	return b.bootstrapperProvider
}

func (b *bootstrapProcessProvider) Provide() (Process, error) {
	b.RLock()
	defer b.RUnlock()
	bootstrapper, err := b.bootstrapperProvider.Provide()
	if err != nil {
		return nil, err
	}

	initialTopologyState, err := b.newInitialTopologyState()
	if err != nil {
		return nil, err
	}

	return bootstrapProcess{
		processOpts:          b.processOpts,
		resultOpts:           b.resultOpts,
		nowFn:                b.resultOpts.ClockOptions().NowFn(),
		log:                  b.log,
		bootstrapper:         bootstrapper,
		initialTopologyState: initialTopologyState,
	}, nil
}

func (b *bootstrapProcessProvider) newInitialTopologyState() (*topology.StateSnapshot, error) {
	topoMap, err := b.processOpts.TopologyMapProvider().TopologyMap()
	if err != nil {
		return nil, err
	}

	var (
		hostShardSets = topoMap.HostShardSets()
		topologyState = &topology.StateSnapshot{
			Origin:           b.processOpts.Origin(),
			MajorityReplicas: topoMap.MajorityReplicas(),
			ShardStates:      topology.ShardStates{},
		}
	)

	for _, hostShardSet := range hostShardSets {
		for _, currShard := range hostShardSet.ShardSet().All() {
			shardID := topology.ShardID(currShard.ID())
			existing, ok := topologyState.ShardStates[shardID]
			if !ok {
				existing = map[topology.HostID]topology.HostShardState{}
				topologyState.ShardStates[shardID] = existing
			}

			hostID := topology.HostID(hostShardSet.Host().ID())
			existing[hostID] = topology.HostShardState{
				Host:       hostShardSet.Host(),
				ShardState: currShard.State(),
			}
		}
	}

	return topologyState, nil
}

type bootstrapProcess struct {
	processOpts          ProcessOptions
	resultOpts           result.Options
	nowFn                clock.NowFn
	log                  *zap.Logger
	bootstrapper         Bootstrapper
	initialTopologyState *topology.StateSnapshot
}

func (b bootstrapProcess) Run(
	at time.Time,
	namespaces []ProcessNamespace,
	shards sharding.ShardSet,
) (NamespaceResults, error) {
	namespacesRunFirst := Namespaces{
		Namespaces: NewNamespacesMap(NamespacesMapOptions{}),
	}
	namespacesRunSecond := Namespaces{
		Namespaces: NewNamespacesMap(NamespacesMapOptions{}),
	}
	for _, namespace := range namespaces {
		ropts := namespace.Metadata.Options().RetentionOptions()
		idxopts := namespace.Metadata.Options().IndexOptions()
		dataRangeFirst, dataRangeSecond := b.targetRangesForData(at, ropts)
		indexRangeFirst, indexRangeSecond := b.targetRangesForIndex(at, ropts, idxopts)

		namespacesRunFirst.Namespaces.Set(namespace.Metadata.ID(), Namespace{
			Metadata:         namespace.Metadata,
			Shards:           shards,
			DataAccumulator:  namespace.DataAccumulator,
			DataTargetRange:  dataRangeFirst,
			IndexTargetRange: indexRangeFirst,
			DataRunOptions: NamespaceRunOptions{
				ShardTimeRanges: b.newShardTimeRanges(dataRangeFirst.Range,
					shards.AllIDs()),
				RunOptions: dataRangeFirst.RunOptions,
			},
			IndexRunOptions: NamespaceRunOptions{
				ShardTimeRanges: b.newShardTimeRanges(indexRangeFirst.Range,
					shards.AllIDs()),
				RunOptions: indexRangeFirst.RunOptions,
			},
		})
		namespacesRunFirst.Namespaces.Set(namespace.Metadata.ID(), Namespace{
			Metadata:         namespace.Metadata,
			Shards:           shards,
			DataAccumulator:  namespace.DataAccumulator,
			DataTargetRange:  dataRangeSecond,
			IndexTargetRange: indexRangeSecond,
			DataRunOptions: NamespaceRunOptions{
				ShardTimeRanges: b.newShardTimeRanges(dataRangeSecond.Range,
					shards.AllIDs()),
				RunOptions: dataRangeSecond.RunOptions,
			},
			IndexRunOptions: NamespaceRunOptions{
				ShardTimeRanges: b.newShardTimeRanges(indexRangeSecond.Range,
					shards.AllIDs()),
				RunOptions: indexRangeSecond.RunOptions,
			},
		})
	}

	bootstrapResult := NewNamespaceResults(namespacesRunFirst)
	for _, namespaces := range []Namespaces{
		namespacesRunFirst,
		namespacesRunSecond,
	} {
		for _, entry := range namespaces.Namespaces.Iter() {
			namespace := entry.Value()
			logFields := b.logFields(namespace.Metadata, shards.AllIDs(),
				namespace.DataTargetRange.Range, namespace.IndexTargetRange.Range)
			b.logBootstrapRun(logFields)
		}

		begin := b.nowFn()
		res, err := b.bootstrapper.Bootstrap(namespaces)
		took := b.nowFn().Sub(begin)

		for _, entry := range namespaces.Namespaces.Iter() {
			namespace := entry.Value()
			logFields := b.logFields(namespace.Metadata, shards.AllIDs(),
				namespace.DataTargetRange.Range, namespace.IndexTargetRange.Range)
			b.logBootstrapResult(logFields, err, took)
		}

		if err != nil {
			return NamespaceResults{}, err
		}

		bootstrapResult = MergeNamespaceResults(bootstrapResult, res)
	}

	return bootstrapResult, nil
}

func (b bootstrapProcess) logFields(
	namespace namespace.Metadata,
	shards []uint32,
	dataTimeWindow xtime.Range,
	indexTimeWindow xtime.Range,
) []zapcore.Field {
	fields := []zapcore.Field{
		zap.String("bootstrapper", b.bootstrapper.String()),
		zap.Int("numShards", len(shards)),
		zap.Time("dataFrom", dataTimeWindow.Start),
		zap.Time("dataTo", dataTimeWindow.End),
		zap.Duration("dataRange", dataTimeWindow.End.Sub(dataTimeWindow.Start)),
	}
	if namespace.Options().IndexOptions().Enabled() {
		fields = append(fields, []zapcore.Field{
			zap.Time("indexFrom", indexTimeWindow.Start),
			zap.Time("indexTo", indexTimeWindow.End),
			zap.Duration("indexRange", indexTimeWindow.End.Sub(indexTimeWindow.Start)),
		}...)
	}
	return fields
}

func (b bootstrapProcess) newShardTimeRanges(
	window xtime.Range,
	shards []uint32,
) result.ShardTimeRanges {
	shardsTimeRanges := make(result.ShardTimeRanges, len(shards))
	ranges := xtime.NewRanges(window)
	for _, s := range shards {
		shardsTimeRanges[s] = ranges
	}
	return shardsTimeRanges
}

func (b bootstrapProcess) logBootstrapRun(
	logFields []zapcore.Field,
) {
	b.log.Info("bootstrap range starting", logFields...)
}

func (b bootstrapProcess) logBootstrapResult(
	logFields []zapcore.Field,
	err error,
	took time.Duration,
) {
	logFields = append(logFields, zap.Duration("took", took))
	if err != nil {
		logFields = append(logFields, zap.Error(err))
		b.log.Info("bootstrap range completed with error", logFields...)
		return
	}

	b.log.Info("bootstrap range completed successfully", logFields...)
}

func (b bootstrapProcess) targetRangesForData(
	at time.Time,
	ropts retention.Options,
) (TargetRange, TargetRange) {
	return b.targetRanges(at, targetRangesOptions{
		retentionPeriod:       ropts.RetentionPeriod(),
		futureRetentionPeriod: ropts.FutureRetentionPeriod(),
		blockSize:             ropts.BlockSize(),
		bufferPast:            ropts.BufferPast(),
		bufferFuture:          ropts.BufferFuture(),
	})
}

func (b bootstrapProcess) targetRangesForIndex(
	at time.Time,
	ropts retention.Options,
	idxopts namespace.IndexOptions,
) (TargetRange, TargetRange) {
	return b.targetRanges(at, targetRangesOptions{
		retentionPeriod:       ropts.RetentionPeriod(),
		futureRetentionPeriod: ropts.FutureRetentionPeriod(),
		blockSize:             idxopts.BlockSize(),
		bufferPast:            ropts.BufferPast(),
		bufferFuture:          ropts.BufferFuture(),
	})
}

type targetRangesOptions struct {
	retentionPeriod       time.Duration
	futureRetentionPeriod time.Duration
	blockSize             time.Duration
	bufferPast            time.Duration
	bufferFuture          time.Duration
}

func (b bootstrapProcess) targetRanges(
	at time.Time,
	opts targetRangesOptions,
) (TargetRange, TargetRange) {
	start := at.Add(-opts.retentionPeriod).
		Truncate(opts.blockSize)
	midPoint := at.
		Add(-opts.blockSize).
		Add(-opts.bufferPast).
		Truncate(opts.blockSize).
		// NB(r): Since "end" is exclusive we need to add a
		// an extra block size when specifying the end time.
		Add(opts.blockSize)
	cutover := at.Add(opts.bufferFuture).
		Truncate(opts.blockSize).
		Add(opts.blockSize)

	// NB(r): We want the large initial time range bootstrapped to
	// bootstrap with persistence so we don't keep the full raw
	// data in process until we finish bootstrapping which could
	// cause the process to OOM.
	return TargetRange{
			Range: xtime.Range{Start: start, End: midPoint},
			RunOptions: b.newRunOptions().SetPersistConfig(PersistConfig{
				Enabled: true,
				// These blocks are no longer active, so we want to flush them
				// to disk as we receive them so that we don't hold too much
				// data in memory at once.
				FileSetType: persist.FileSetFlushType,
			}),
		}, TargetRange{
			Range: xtime.Range{Start: midPoint, End: cutover},
			RunOptions: b.newRunOptions().SetPersistConfig(PersistConfig{
				Enabled: true,
				// These blocks are still active so we'll have to keep them
				// in memory, but we want to snapshot them as we receive them
				// so that once bootstrapping completes we can still recover
				// from just the commit log bootstrapper.
				FileSetType: persist.FileSetSnapshotType,
			}),
		}
}

func (b bootstrapProcess) newRunOptions() RunOptions {
	return NewRunOptions().
		SetCacheSeriesMetadata(
			b.processOpts.CacheSeriesMetadata(),
		).
		SetInitialTopologyState(b.initialTopologyState)
}

// NewNamespaces returns a new set of bootstrappable namespaces.
func NewNamespaces(
	namespaces []ProcessNamespace,
	shards sharding.ShardSet,
) Namespaces {
	namespacesMap := NewNamespacesMap(NamespacesMapOptions{})
	for _, ns := range namespaces {
		namespacesMap.Set(ns.Metadata.ID(), Namespace{
			Metadata:        ns.Metadata,
			Shards:          shards,
			DataAccumulator: ns.DataAccumulator,
		})
	}
	return Namespaces{
		Namespaces: namespacesMap,
	}
}

// NewNamespaceResults creates a
// namespace results map with an entry for each
// namespace spoecified by a namespaces map.
func NewNamespaceResults(
	namespaces Namespaces,
) NamespaceResults {
	resultsMap := NewNamespaceResultsMap(NamespaceResultsMapOptions{})
	for _, entry := range namespaces.Namespaces.Iter() {
		key := entry.Key()
		value := entry.Value()
		resultsMap.Set(key, NamespaceResult{
			Metadata:    value.Metadata,
			Shards:      value.Shards,
			DataResult:  result.NewDataBootstrapResult(),
			IndexResult: result.NewIndexBootstrapResult(),
		})
	}
	return NamespaceResults{
		Results: resultsMap,
	}
}

// MergeNamespaceResults merges two namespace results.
func MergeNamespaceResults(a, b NamespaceResults) NamespaceResults {
	for _, entry := range a.Results.Iter() {
		id := entry.Key()
		elem := entry.Value()
		other, ok := b.Results.Get(id)
		if !ok {
			continue
		}
		elem.DataResult = result.MergedDataBootstrapResult(elem.DataResult,
			other.DataResult)
		elem.IndexResult = result.MergedIndexBootstrapResult(elem.IndexResult,
			other.IndexResult)

		// Remove from b, then can directly add to a all non-merged results.
		b.Results.Delete(id)
	}
	// All overlapping between a and b have been merged, add rest to a.
	for _, entry := range b.Results.Iter() {
		a.Results.Set(entry.Key(), entry.Value())
	}
	return a
}
