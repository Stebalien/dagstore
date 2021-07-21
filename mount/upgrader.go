package mount

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	logging "github.com/ipfs/go-log/v2"
)

var log = logging.Logger("dagstore-impl")

// Upgrader is a bridge to upgrade any Mount into one with full-featured
// Reader capabilities, whether the original mount is of remote or local kind.
// It does this by managing a local transient copy.
//
// If the underlying mount is fully-featured, the Upgrader has no effect, and
// simply passes through to the underlying mount.
type Upgrader struct {
	rootdir     string
	underlying  Mount
	key         string
	passthrough bool

	lk        sync.Mutex
	transient string
	once      *sync.Once
}

var _ Mount = (*Upgrader)(nil)

// Upgrade constructs a new Upgrader for the underlying Mount. If provided, it
// will reuse the file in path `initial` as the initial transient copy. Whenever
// a new transient copy has to be created, it will be created under `rootdir`.
func Upgrade(underlying Mount, rootdir, key string, initial string) (*Upgrader, error) {
	ret := &Upgrader{underlying: underlying, key: key, rootdir: rootdir, once: new(sync.Once)}
	if ret.rootdir == "" {
		ret.rootdir = os.TempDir() // use the OS' default temp dir.
	}

	switch info := underlying.Info(); {
	case !info.AccessSequential:
		return nil, fmt.Errorf("underlying mount must support sequential access")
	case info.AccessSeek && info.AccessRandom:
		ret.passthrough = true
		return ret, nil
	}

	if initial != "" {
		if _, err := os.Stat(initial); err == nil {
			ret.transient = initial
			return ret, nil
		}
	}

	return ret, nil
}

func (u *Upgrader) Fetch(ctx context.Context) (Reader, error) {
	if u.passthrough {
		log.Info("allowing pass-through fetch")
		return u.underlying.Fetch(ctx)
	}

	// determine if the transient is still alive.
	// if not, get the current sync.Once and trigger a refresh.
	// after it's done, open the resulting transient.
	u.lk.Lock()
	if u.transient != "" {
		if _, err := os.Stat(u.transient); err == nil {
			defer u.lk.Unlock()
			log.Infow("will open existing transient on upgrader fetch", "transient path", u.transient)
			return os.Open(u.transient)
		}
		// TODO add size check.
	}
	// transient appears to be dead, refetch.
	// get the current sync under the lock, use it to deduplicate concurrent fetches.
	once := u.once
	u.lk.Unlock()

	var err error
	once.Do(func() { err = u.refetch(ctx) })

	if err != nil {
		return nil, fmt.Errorf("mount fetch failed: %w", err)
	}

	u.lk.Lock()
	defer u.lk.Unlock()

	log.Infow("will open fetched transient on upgrader fetch", "transient path", u.transient)
	return os.Open(u.transient)
}

func (u *Upgrader) Info() Info {
	return Info{
		Kind:             KindLocal,
		AccessSequential: true,
		AccessSeek:       true,
		AccessRandom:     true,
	}
}

func (u *Upgrader) Stat(ctx context.Context) (Stat, error) {
	if u.transient != "" {
		if stat, err := os.Stat(u.transient); err == nil {
			ret := Stat{Exists: true, Size: stat.Size()}
			return ret, nil
		}
	}
	return u.underlying.Stat(ctx)
}

// TransientPath returns the local path of the transient file. If the Upgrader
// is passthrough, the return value will be "".
func (u *Upgrader) TransientPath() string {
	u.lk.Lock()
	defer u.lk.Unlock()

	return u.transient
}

func (u *Upgrader) Serialize() *url.URL {
	return u.underlying.Serialize()
}

func (u *Upgrader) Deserialize(url *url.URL) error {
	return u.underlying.Deserialize(url)
}

func (u *Upgrader) Close() error {
	panic("implement me")
}

func (u *Upgrader) refetch(ctx context.Context) error {
	u.lk.Lock()
	if u.transient != "" {
		log.Infow("removing existing transient", "transient path", u.transient)
		_ = os.Remove(u.transient)
	}
	u.lk.Unlock()

	file, err := os.CreateTemp(u.rootdir, "transient-"+u.key+"-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer file.Close()

	// sanity check on underlying mount.
	if stat, err := u.underlying.Stat(ctx); err != nil {
		return fmt.Errorf("underlying mount stat returned error: %w", err)
	} else if !stat.Exists {
		return fmt.Errorf("underlying mount no longer exists")
	}

	// fetch from underlying and copy.
	from, err := u.underlying.Fetch(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch from underlying mount: %w", err)
	}
	defer from.Close()

	_, err = io.Copy(file, from)
	if err != nil {
		return fmt.Errorf("failed to copy underlying mount to transient file: %w", err)
	}

	// set the new transient path under a lock, and recycle the sync.Once.
	u.lk.Lock()
	log.Infow("updating transient path", "new path", file.Name())
	u.transient = file.Name()
	u.once = new(sync.Once)
	u.lk.Unlock()

	return nil
}

// DeleteTransient deletes the transient associated with this Upgrader, if
// one exists. It is the caller's responsibility to ensure the transient is
// not in use. If the tracked transient is gone, this will reset the internal
// state to "" (no transient) to enable recovery.
func (u *Upgrader) DeleteTransient() error {
	log.Info("will acquire GC transient lock")
	u.lk.Lock()
	log.Info("acquired GC transient lock")
	defer u.lk.Unlock()

	if u.transient == "" {
		return nil // nothing to do.
	}

	// refuse to delete the transient if it's not being managed by us (i.e. in
	// our transients root directory).
	if _, err := filepath.Rel(u.rootdir, u.transient); err != nil {
		return nil
	}

	// remove the transient and clear it always, even if os.Remove
	// returns an error. This allows us to recover from errors like the user
	// deleting the transient we're currently tracking.
	log.Infow("removing transient in GC", "transient path", u.transient)
	err := os.Remove(u.transient)
	u.transient = ""
	return err
}
