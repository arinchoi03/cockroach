// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Matt Tracy (matt@cockroachlabs.com)

package storage

import (
	"time"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

const (
	// TimeSeriesMaintenanceInterval is the minimum interval between two
	// time series maintenance runs on a replica.
	TimeSeriesMaintenanceInterval     = 24 * time.Hour // daily
	timeSeriesMaintenanceQueueMaxSize = 100
)

// TimeSeriesDataStore is an interface defined in the storage package that can
// be implemented by the higher-level time series system. This allows the
// storage queues to run periodic time series maintenance; importantly, this
// maintenance can then be informed by data from the local store.
type TimeSeriesDataStore interface {
	ContainsTimeSeries(roachpb.RKey, roachpb.RKey) bool
	PruneTimeSeries(
		context.Context, engine.Reader, roachpb.RKey, roachpb.RKey, *client.DB, hlc.Timestamp,
	) error
}

// timeSeriesMaintenanceQueue identifies replicas that contain time series
// data and performs necessary data maintenance on the time series located in
// the replica. Currently, maintenance involves pruning time series data older
// than a certain threshold.
//
// Logic for time series maintenance is implemented in a higher level time
// series package; this queue uses the TimeSeriesDataStore interface to call
// into that logic.
//
// Once a replica is selected for processing, data changes are executed against
// the cluster using a KV client; changes are not restricted to the data in the
// replica being processed. These tasks could therefore be performed without a
// replica queue; however, a replica queue has been chosen to initiate this task
// for a few reasons:
// * The queue naturally distributes the workload across the cluster in
// proportion to the number of ranges containing time series data.
// * Access to the local replica is a convenient way to discover the names of
// time series stored on the cluster; the names are required in order to
// effectively prune time series without expensive distributed scans.
//
// Data changes executed by this queue are idempotent; it is explicitly safe
// for multiple nodes to attempt to prune the same time series concurrently.
// In this situation, each node would compute the same delete range based on
// the current timestamp; the first will succeed, all others will become
// a no-op.
type timeSeriesMaintenanceQueue struct {
	*baseQueue
	tsData         TimeSeriesDataStore
	replicaCountFn func() int
	db             *client.DB
}

// newTimeSeriesMaintenanceQueue returns a new instance of
// timeSeriesMaintenanceQueue.
func newTimeSeriesMaintenanceQueue(
	store *Store, db *client.DB, g *gossip.Gossip, tsData TimeSeriesDataStore,
) *timeSeriesMaintenanceQueue {
	q := &timeSeriesMaintenanceQueue{
		tsData:         tsData,
		replicaCountFn: store.ReplicaCount,
		db:             db,
	}
	q.baseQueue = newBaseQueue(
		"timeSeriesMaintenance", q, store, g,
		queueConfig{
			maxSize:              timeSeriesMaintenanceQueueMaxSize,
			needsLease:           true,
			acceptsUnsplitRanges: true,
			successes:            store.metrics.TimeSeriesMaintenanceQueueSuccesses,
			failures:             store.metrics.TimeSeriesMaintenanceQueueFailures,
			pending:              store.metrics.TimeSeriesMaintenanceQueuePending,
			processingNanos:      store.metrics.TimeSeriesMaintenanceQueueProcessingNanos,
		},
	)

	return q
}

func (q *timeSeriesMaintenanceQueue) shouldQueue(
	ctx context.Context, now hlc.Timestamp, repl *Replica, _ config.SystemConfig,
) (shouldQ bool, priority float64) {
	if !repl.store.cfg.TestingKnobs.DisableLastProcessedCheck {
		lpTS, err := repl.getQueueLastProcessed(ctx, q.name)
		if err != nil {
			log.ErrEventf(ctx, "time series maintenance queue last processed timestamp: %s", err)
		}
		shouldQ, priority = shouldQueueAgain(now, lpTS, TimeSeriesMaintenanceInterval)
		if !shouldQ {
			return
		}
	}
	desc := repl.Desc()
	if q.tsData.ContainsTimeSeries(desc.StartKey, desc.EndKey) {
		return
	}
	return false, 0
}

func (q *timeSeriesMaintenanceQueue) process(
	ctx context.Context, repl *Replica, sysCfg config.SystemConfig,
) error {
	desc := repl.Desc()
	snap := repl.store.Engine().NewSnapshot()
	now := repl.store.Clock().Now()
	defer snap.Close()
	if err := q.tsData.PruneTimeSeries(ctx, snap, desc.StartKey, desc.EndKey, q.db, now); err != nil {
		return err
	}
	// Update the last processed time for this queue.
	if err := repl.setQueueLastProcessed(ctx, q.name, now); err != nil {
		log.ErrEventf(ctx, "failed to update last processed time: %v", err)
	}
	return nil
}

func (q *timeSeriesMaintenanceQueue) timer(duration time.Duration) time.Duration {
	// An interval between replicas to space consistency checks out over
	// the check interval.
	replicaCount := q.replicaCountFn()
	if replicaCount == 0 {
		return 0
	}
	replInterval := TimeSeriesMaintenanceInterval / time.Duration(replicaCount)
	if replInterval < duration {
		return 0
	}
	return replInterval - duration
}

func (*timeSeriesMaintenanceQueue) purgatoryChan() <-chan struct{} {
	return nil
}
