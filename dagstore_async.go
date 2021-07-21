package dagstore

import (
	"context"

	"github.com/filecoin-project/dagstore/mount"
	"github.com/ipld/go-car/v2"
	"github.com/ipld/go-car/v2/index"
)

//
// This file contains methods that are called from the event loop
// but are run asynchronously in dedicated goroutines.
//

// acquireAsync acquires a shard by fetching its data, obtaining its index, and
// joining them to form a ShardAccessor.
func (d *DAGStore) acquireAsync(ctx context.Context, w *waiter, s *Shard, mnt mount.Mount) {
	k := s.key

	var reader mount.Reader
	log.Info("will call throttleFetch in acquireAsync")
	err := d.throttleFetch.Do(ctx, func(ctx context.Context) error {
		log.Info("throttleFetch called in acquireAsync")
		var err error
		reader, err = mnt.Fetch(ctx)
		log.Infow("finished mnt fetch", "err", err)
		return err
	})

	if err != nil {
		// release the shard to decrement the refcount that's incremented before `acquireAsync` is called.
		_ = d.queueTask(&task{op: OpShardRelease, shard: s}, d.completionCh)

		// fail the shard
		_ = d.failShard(s, d.completionCh, "failed to acquire reader of mount: %w", err)

		// send the shard error to the caller.
		d.dispatchResult(&ShardResult{Key: k, Error: err}, w)
		return
	}

	idx, err := d.indices.GetFullIndex(k)
	if err != nil {
		log.Errorw("error fetching index", "err", err)
		if err := reader.Close(); err != nil {
			log.Errorf("failed to close mount reader: %s", err)
		}

		// release the shard to decrement the refcount that's incremented before `acquireAsync` is called.
		_ = d.queueTask(&task{op: OpShardRelease, shard: s}, d.completionCh)

		// fail the shard
		_ = d.failShard(s, d.completionCh, "failed to recover index for shard %s: %w", k, err)

		// send the shard error to the caller.
		d.dispatchResult(&ShardResult{Key: k, Error: err}, w)
		return
	}

	sa, err := NewShardAccessor(reader, idx, s)

	// send the shard accessor to the caller.
	d.dispatchResult(&ShardResult{Key: k, Accessor: sa, Error: err}, w)
}

// initializeShard initializes a shard asynchronously by fetching its data and
// performing indexing.
func (d *DAGStore) initializeShard(ctx context.Context, s *Shard, mnt mount.Mount) {
	var reader mount.Reader
	log.Info("will call throttleFetch in initializeShard")

	err := d.throttleFetch.Do(ctx, func(ctx context.Context) error {
		log.Info("throttleFetch called in initializeShard")

		var err error
		reader, err = mnt.Fetch(ctx)
		return err
	})

	if err != nil {
		_ = d.failShard(s, d.completionCh, "failed to acquire reader of mount: %w", err)
		return
	}
	defer reader.Close()

	// works for both CARv1 and CARv2.
	var idx index.Index
	err = d.throttleIndex.Do(ctx, func(_ context.Context) error {
		var err error
		idx, err = car.ReadOrGenerateIndex(reader, car.ZeroLengthSectionAsEOF(true))
		return err
	})
	if err != nil {
		_ = d.failShard(s, d.completionCh, "failed to read/generate CAR Index: %w", err)
		return
	}
	if err := d.indices.AddFullIndex(s.key, idx); err != nil {
		_ = d.failShard(s, d.completionCh, "failed to add index for shard: %w", err)
		return
	}

	_ = d.queueTask(&task{op: OpShardMakeAvailable, shard: s}, d.completionCh)
}
