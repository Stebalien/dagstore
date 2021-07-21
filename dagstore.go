package dagstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/filecoin-project/dagstore/index"
	"github.com/filecoin-project/dagstore/mount"
	"github.com/filecoin-project/dagstore/shard"
	"github.com/hashicorp/go-multierror"
	ds "github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	"github.com/ipfs/go-datastore/query"
	dssync "github.com/ipfs/go-datastore/sync"
	logging "github.com/ipfs/go-log/v2"
)

var (
	// StoreNamespace is the namespace under which shard state will be persisted.
	StoreNamespace = ds.NewKey("dagstore")
)

var log = logging.Logger("dagstore")

var (
	// ErrShardUnknown is the error returned when the requested shard is
	// not known to the DAG store.
	ErrShardUnknown = errors.New("shard not found")

	// ErrShardExists is the error returned upon registering a duplicate shard.
	ErrShardExists = errors.New("shard already exists")

	// ErrShardInitializationFailed is returned when shard initialization fails.
	ErrShardInitializationFailed = errors.New("shard initialization failed")

	// ErrShardInUse is returned when the user attempts to destroy a shard that
	// is in use.
	ErrShardInUse = errors.New("shard in use")
)

// DAGStore is the central object of the DAG store.
type DAGStore struct {
	lk      sync.RWMutex
	mounts  *mount.Registry
	shards  map[shard.Key]*Shard
	config  Config
	indices index.FullIndexRepo
	store   ds.Datastore

	// Channels owned by us.
	//
	// externalCh receives external tasks.
	externalCh chan *task
	// internalCh receives internal tasks to the event loop.
	internalCh chan *task
	// completionCh receives tasks queued up as a result of async completions.
	completionCh chan *task
	// dispatchResultsCh is a buffered channel for dispatching results back to
	// the application. Serviced by a dispatcher goroutine.
	// Note: This pattern decouples the event loop from the application, so a
	// failure to consume immediately won't block the event loop.
	dispatchResultsCh chan *dispatch
	// dispatchFailuresCh is a buffered channel for dispatching shard failures
	// back to the application. Serviced by a dispatcher goroutine.
	// See note in dispatchResultsCh for background.
	dispatchFailuresCh chan *dispatch

	// Channels not owned by us.
	//
	// traceCh is where traces on shard operations will be sent, if non-nil.
	traceCh chan<- Trace
	// failureCh is where shard failures will be notified, if non-nil.
	failureCh chan<- ShardResult

	// Throttling.
	//
	throttleFetch Throttler
	throttleIndex Throttler

	// Lifecycle.
	//
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

type dispatch struct {
	w   *waiter
	res *ShardResult
}

// Task represents an operation to be performed on a shard or the DAG store.
type task struct {
	*waiter
	op    OpType
	shard *Shard
	err   error
}

// ShardResult encapsulates a result from an asynchronous operation.
type ShardResult struct {
	Key      shard.Key
	Error    error
	Accessor *ShardAccessor
}

type Config struct {
	// TransientsDir is the path to directory where local transient files will
	// be created for remote mounts.
	TransientsDir string

	// IndexDir is the path where indices are stored.
	IndexDir string

	// Datastore is the datastore where shard state will be persisted.
	Datastore ds.Datastore

	// MountRegistry contains the set of recognized mount types.
	MountRegistry *mount.Registry

	// TraceCh is a channel where the caller desires to be notified of every
	// shard operation. Publishing to this channel blocks the event loop, so the
	// caller must ensure the channel is serviced appropriately.
	TraceCh chan<- Trace

	// FailureCh is a channel to be notified every time that a shard moves to
	// ShardStateErrored. A nil value will send no failure notifications.
	// Failure events can be used to evaluate the error and call
	// DAGStore.RecoverShard if deemed recoverable.
	FailureCh chan<- ShardResult

	// MaxConcurrentIndex is the maximum indexing jobs that can
	// run concurrently. 0 (default) disables throttling.
	MaxConcurrentIndex int

	// MaxConcurrentFetch is the maximum fetching jobs that can
	// run concurrently. 0 (default) disables throttling.
	MaxConcurrentFetch int
}

// NewDAGStore constructs a new DAG store with the supplied configuration.
func NewDAGStore(cfg Config) (*DAGStore, error) {
	// validate and manage scratch root directory.
	if cfg.TransientsDir == "" {
		return nil, fmt.Errorf("missing scratch area root path")
	}
	if err := ensureDir(cfg.TransientsDir); err != nil {
		return nil, fmt.Errorf("failed to create scratch root dir: %w", err)
	}

	// instantiate the index repo.
	var indices index.FullIndexRepo
	if cfg.IndexDir == "" {
		log.Info("using in-memory index store")
		indices = index.NewMemoryRepo()
	} else {
		err := ensureDir(cfg.IndexDir)
		if err != nil {
			return nil, fmt.Errorf("failed to create index root dir: %w", err)
		}
		indices, err = index.NewFSRepo(cfg.IndexDir)
		if err != nil {
			return nil, fmt.Errorf("failed to instantiate full index repo: %w", err)
		}
	}

	// handle the datastore.
	if cfg.Datastore == nil {
		log.Warnf("no datastore provided; falling back to in-mem datastore; shard state will not survive restarts")
		cfg.Datastore = dssync.MutexWrap(ds.NewMapDatastore()) // TODO can probably remove mutex wrap, since access is single-threaded
	}

	// namespace all store operations.
	cfg.Datastore = namespace.Wrap(cfg.Datastore, StoreNamespace)

	if cfg.MountRegistry == nil {
		cfg.MountRegistry = mount.NewRegistry()
	}

	ctx, cancel := context.WithCancel(context.Background())
	dagst := &DAGStore{
		mounts:            cfg.MountRegistry,
		config:            cfg,
		indices:           indices,
		shards:            make(map[shard.Key]*Shard),
		store:             cfg.Datastore,
		externalCh:        make(chan *task, 128),     // len=128, concurrent external tasks that can be queued up before exercising backpressure.
		internalCh:        make(chan *task, 1),       // len=1, because eventloop will only ever stage another internal event.
		completionCh:      make(chan *task, 64),      // len=64, hitting this limit will just make async tasks wait.
		dispatchResultsCh: make(chan *dispatch, 128), // len=128, same as externalCh.
		traceCh:           cfg.TraceCh,
		failureCh:         cfg.FailureCh,
		throttleFetch:     noopThrottler{},
		throttleIndex:     noopThrottler{},
		ctx:               ctx,
		cancelFn:          cancel,
	}

	if max := cfg.MaxConcurrentFetch; max > 0 {
		dagst.throttleFetch = NewThrottler(max)
	}

	if max := cfg.MaxConcurrentIndex; max > 0 {
		dagst.throttleIndex = NewThrottler(max)
	}

	if err := dagst.restoreState(); err != nil {
		// TODO add a lenient mode.
		return nil, fmt.Errorf("failed to restore dagstore state: %w", err)
	}

	// Reset in-progress states.
	//
	// Queue shards whose registration needs to be restarted. Release those
	// ops after we spawn the control goroutine. Otherwise, having more shards
	// in this state than the externalCh buffer size would exceed the channel
	// buffer, and we'd block forever.
	var register []*Shard
	for _, s := range dagst.shards {
		// reset to available, as we have no active acquirers at start.
		if s.state == ShardStateServing {
			s.state = ShardStateAvailable
		}

		// Note: An available shard whose index has disappeared across restarts
		// will fail on the first acquisition.

		// handle shards that were initializing when we shut down.
		if s.state == ShardStateInitializing {
			// if we already have the index for the shard, there's nothing else to do.
			if istat, err := dagst.indices.StatFullIndex(s.key); err == nil && istat.Exists {
				s.state = ShardStateAvailable
			} else {
				// reset back to new, and queue the OpShardRegister.
				s.state = ShardStateNew
				register = append(register, s)
			}
		}
	}

	// spawn the control goroutine.
	dagst.wg.Add(1)
	go dagst.control()

	// spawn the dispatcher goroutine for responses, responsible for pumping
	// async results back to the caller.
	dagst.wg.Add(1)
	go dagst.dispatcher(dagst.dispatchResultsCh)

	// application has provided a failure channel; spawn the dispatcher.
	if dagst.failureCh != nil {
		dagst.dispatchFailuresCh = make(chan *dispatch, 128) // len=128, same as externalCh.
		dagst.wg.Add(1)
		go dagst.dispatcher(dagst.dispatchFailuresCh)
	}

	// release the queued registrations before we return.
	for _, s := range register {
		_ = dagst.queueTask(&task{op: OpShardRegister, shard: s, waiter: &waiter{ctx: ctx}}, dagst.externalCh)
	}

	return dagst, nil
}

type RegisterOpts struct {
	// ExistingTransient can be supplied when registering a shard to indicate
	// that there's already an existing local transient copy that can be used
	// for indexing.
	ExistingTransient string

	// LazyInitialization defers shard indexing to the first access instead of
	// performing it at registration time. Use this option when fetching the
	// asset is expensive.
	//
	// When true, the registration channel will fire as soon as the DAG store
	// has acknowledged the inclusion of the shard, without waiting for any
	// indexing to happen.
	LazyInitialization bool
}

// RegisterShard initiates the registration of a new shard.
//
// This method returns an error synchronously if preliminary validation fails.
// Otherwise, it queues the shard for registration. The caller should monitor
// supplied channel for a result.
func (d *DAGStore) RegisterShard(ctx context.Context, key shard.Key, mnt mount.Mount, out chan ShardResult, opts RegisterOpts) error {
	d.lk.Lock()
	if _, ok := d.shards[key]; ok {
		d.lk.Unlock()
		return fmt.Errorf("%s: %w", key.String(), ErrShardExists)
	}

	// wrap the original mount in an upgrader.
	upgraded, err := mount.Upgrade(mnt, d.config.TransientsDir, key.String(), opts.ExistingTransient)
	if err != nil {
		d.lk.Unlock()
		return err
	}

	w := &waiter{outCh: out, ctx: ctx}

	// add the shard to the shard catalogue, and drop the lock.
	s := &Shard{
		d:     d,
		key:   key,
		state: ShardStateNew,
		mount: upgraded,
		lazy:  opts.LazyInitialization,
	}
	d.shards[key] = s
	d.lk.Unlock()

	tsk := &task{op: OpShardRegister, shard: s, waiter: w}
	return d.queueTask(tsk, d.externalCh)
}

type DestroyOpts struct {
}

func (d *DAGStore) DestroyShard(ctx context.Context, key shard.Key, out chan ShardResult, _ DestroyOpts) error {
	d.lk.Lock()
	s, ok := d.shards[key]
	if !ok {
		d.lk.Unlock()
		return ErrShardUnknown // TODO: encode shard key
	}
	d.lk.Unlock()

	tsk := &task{op: OpShardDestroy, shard: s, waiter: &waiter{ctx: ctx, outCh: out}}
	return d.queueTask(tsk, d.externalCh)
}

type AcquireOpts struct {
}

// AcquireShard acquires access to the specified shard, and returns a
// ShardAccessor, an object that enables various patterns of access to the data
// contained within the shard.
//
// This operation may resolve near-instantaneously if the shard is available
// locally. If not, the shard data may be fetched from its mount.
//
// This method returns an error synchronously if preliminary validation fails.
// Otherwise, it queues the shard for acquisition. The caller should monitor
// supplied channel for a result.
func (d *DAGStore) AcquireShard(ctx context.Context, key shard.Key, out chan ShardResult, _ AcquireOpts) error {
	log.Info("will acquire dasg store lock")
	d.lk.Lock()
	s, ok := d.shards[key]
	if !ok {
		d.lk.Unlock()
		return fmt.Errorf("%s: %w", key.String(), ErrShardUnknown)
	}
	d.lk.Unlock()
	log.Info("released dagstore lock")

	tsk := &task{op: OpShardAcquire, shard: s, waiter: &waiter{ctx: ctx, outCh: out}}
	log.Info("will send message to acquire shard")
	return d.queueTask(tsk, d.externalCh)
}

type RecoverOpts struct {
}

// RecoverShard recovers a shard in ShardStateErrored state.
//
// If the shard referenced by the key doesn't exist, an error is returned
// immediately and no result is delivered on the supplied channel.
//
// If the shard is not in the ShardStateErrored state, the operation is accepted
// but an error will be returned quickly on the supplied channel.
//
// Otherwise, the recovery operation will be queued and the supplied channel
// will be notified when it completes.
//
// TODO add an operation identifier to ShardResult -- starts to look like
//  a Trace event?
func (d *DAGStore) RecoverShard(ctx context.Context, key shard.Key, out chan ShardResult, _ RecoverOpts) error {
	d.lk.Lock()
	s, ok := d.shards[key]
	if !ok {
		d.lk.Unlock()
		return fmt.Errorf("%s: %w", key.String(), ErrShardUnknown)
	}
	d.lk.Unlock()

	tsk := &task{op: OpShardRecover, shard: s, waiter: &waiter{ctx: ctx, outCh: out}}
	return d.queueTask(tsk, d.externalCh)
}

type Trace struct {
	Key   shard.Key
	Op    OpType
	After ShardInfo
}

type ShardInfo struct {
	ShardState
	Error error
	refs  uint32
}

// GetShardInfo returns the current state of shard with key k.
//
// If the shard is not known, ErrShardUnknown is returned.
func (d *DAGStore) GetShardInfo(k shard.Key) (ShardInfo, error) {
	d.lk.RLock()
	defer d.lk.RUnlock()
	s, ok := d.shards[k]
	if !ok {
		return ShardInfo{}, ErrShardUnknown
	}

	s.lk.RLock()
	info := ShardInfo{ShardState: s.state, Error: s.err, refs: s.refs}
	s.lk.RUnlock()
	return info, nil
}

type AllShardsInfo map[shard.Key]ShardInfo

// AllShardsInfo returns the current state of all registered shards, as well as
// any errors.
func (d *DAGStore) AllShardsInfo() AllShardsInfo {
	d.lk.RLock()
	defer d.lk.RUnlock()

	ret := make(AllShardsInfo, len(d.shards))
	for k, s := range d.shards {
		s.lk.RLock()
		info := ShardInfo{ShardState: s.state, Error: s.err, refs: s.refs}
		s.lk.RUnlock()
		ret[k] = info
	}
	return ret
}

// GC attempts to reclaim the transient files of shards that are currently
// available but inactive.
//
// It is not strictly atomic for now, as it determines which shards to reclaim
// first, sends operations to the event loop, and waits for them to execute.
// In the meantime, there could be state transitions that change reclaimability
// of shards (some shards deemed reclaimable are no longer so, and vice versa).
//
// However, the event loop checks for safety prior to deletion, so it will skip
// over shards that are no longer safe to delete.
func (d *DAGStore) GC(ctx context.Context) (map[shard.Key]error, error) {
	var (
		merr    *multierror.Error
		reclaim []*Shard
	)

	d.lk.RLock()
	for _, s := range d.shards {
		s.lk.RLock()
		if s.state == ShardStateAvailable || s.state == ShardStateErrored {
			reclaim = append(reclaim, s)
		}
		s.lk.RUnlock()
	}
	d.lk.RUnlock()

	var await int
	ch := make(chan ShardResult, len(reclaim))
	for _, s := range reclaim {
		tsk := &task{op: OpShardGC, shard: s, waiter: &waiter{ctx: ctx, outCh: ch}}

		err := d.queueTask(tsk, d.externalCh)
		if err == nil {
			await++
		} else {
			merr = multierror.Append(merr, fmt.Errorf("failed to enqueue GC task for shard %s: %w", s.key, err))
		}
	}

	// collect all results.
	results := make(map[shard.Key]error, await)
	for i := 0; i < await; i++ {
		select {
		case res := <-ch:
			results[res.Key] = res.Error
		case <-ctx.Done():
			return results, ctx.Err()
		}
	}

	return results, nil
}

func (d *DAGStore) Close() error {
	d.cancelFn()
	d.wg.Wait()
	_ = d.store.Sync(ds.Key{})
	return nil
}

func (d *DAGStore) queueTask(tsk *task, ch chan<- *task) error {
	if tsk.op == OpShardAcquire {
		log.Info("in queueTask for OpShardAcquire")
	}

	select {
	case <-d.ctx.Done():
		return fmt.Errorf("dag store closed")
	case ch <- tsk:
		if tsk.op == OpShardAcquire {
			log.Info("finished writing OpShardAcquire task to channel")
		}
		return nil
	}
}

func (d *DAGStore) restoreState() error {
	results, err := d.store.Query(query.Query{})
	if err != nil {
		return fmt.Errorf("failed to recover dagstore state from store: %w", err)
	}
	for {
		res, ok := results.NextSync()
		if !ok {
			return nil
		}
		s := &Shard{d: d}
		if err := s.UnmarshalJSON(res.Value); err != nil {
			log.Warnf("failed to recover state of shard %s: %s; skipping", shard.KeyFromString(res.Key), err)
			continue
		}
		d.shards[s.key] = s
	}
}

// ensureDir checks whether the specified path is a directory, and if not it
// attempts to create it.
func ensureDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		// We need to create the directory.
		return os.MkdirAll(path, os.ModePerm)
	}

	if !fi.IsDir() {
		return fmt.Errorf("path %s exists, and it is not a directory", path)
	}
	return nil
}

// failShard queues a shard failure (does not fail it immediately). It is
// suitable for usage both outside and inside the event loop, depending on the
// channel passed.
func (d *DAGStore) failShard(s *Shard, ch chan *task, format string, args ...interface{}) error {
	err := fmt.Errorf(format, args...)
	return d.queueTask(&task{op: OpShardFail, shard: s, err: err}, ch)
}
