package cniprovider

import (
	"context"
	"os"
	"runtime"
	"sync"

	cni "github.com/containerd/go-cni"
	"github.com/gofrs/flock"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/util/network"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/trace"
)

type Opt struct {
	Root       string
	ConfigPath string
	BinaryDir  string
	PoolSize   int
}

func New(opt Opt) (network.Provider, error) {
	if _, err := os.Stat(opt.ConfigPath); err != nil {
		return nil, errors.Wrapf(err, "failed to read cni config %q", opt.ConfigPath)
	}
	if _, err := os.Stat(opt.BinaryDir); err != nil {
		return nil, errors.Wrapf(err, "failed to read cni binary dir %q", opt.BinaryDir)
	}

	cniOptions := []cni.Opt{cni.WithPluginDir([]string{opt.BinaryDir}), cni.WithInterfacePrefix("eth")}

	// Windows doesn't use CNI for loopback.
	if runtime.GOOS != "windows" {
		cniOptions = append([]cni.Opt{cni.WithMinNetworkCount(2)}, cniOptions...)
		cniOptions = append(cniOptions, cni.WithLoNetwork)
	}

	cniOptions = append(cniOptions, cni.WithConfFile(opt.ConfigPath))

	cniHandle, err := cni.New(cniOptions...)
	if err != nil {
		return nil, err
	}

	cp := &cniProvider{CNI: cniHandle, root: opt.Root}
	cp.nsPool = &cniPool{maxSize: opt.PoolSize, provider: cp}
	if err := cp.initNetwork(); err != nil {
		return nil, err
	}
	return cp, nil
}

type cniProvider struct {
	cni.CNI
	root   string
	nsPool *cniPool
}

func (c *cniProvider) initNetwork() error {
	if v := os.Getenv("BUILDKIT_CNI_INIT_LOCK_PATH"); v != "" {
		l := flock.New(v)
		if err := l.Lock(); err != nil {
			return err
		}
		defer l.Unlock()
	}
	ns, err := c.New(nil)
	if err != nil {
		return err
	}
	return ns.Close()
}

type cniPool struct {
	maxSize   int
	provider  *cniProvider
	mu        sync.Mutex
	total     int
	available []*cniNS
}

func (pool *cniPool) Get(ctx context.Context) (*cniNS, error) {
	pool.mu.Lock()
	// Lazily grow the pool to its max size
	if pool.total >= pool.maxSize && len(pool.available) > 0 {
		trace.SpanFromContext(ctx).AddEvent("returning net NS from pool")
		ns := pool.available[0]
		pool.available = pool.available[1:]
		pool.mu.Unlock()
		return ns, nil
	}
	pool.mu.Unlock()

	id := identity.NewID()
	trace.SpanFromContext(ctx).AddEvent("cniProvider generated ID")
	nativeID, err := createNetNS(ctx, pool.provider, id)
	if err != nil {
		return nil, err
	}
	trace.SpanFromContext(ctx).AddEvent("finished createNetNS")

	if _, err := pool.provider.CNI.Setup(context.TODO(), id, nativeID); err != nil {
		deleteNetNS(nativeID)
		return nil, errors.Wrap(err, "CNI setup error")
	}
	trace.SpanFromContext(ctx).AddEvent("finished cni Setup")

	pool.mu.Lock()
	pool.total++
	pool.mu.Unlock()
	return &cniNS{pool: pool, nativeID: nativeID, id: id, handle: pool.provider.CNI}, nil

}

func (pool *cniPool) Put(ns *cniNS) error {
	pool.mu.Lock()
	if len(pool.available) < pool.maxSize {
		pool.available = append(pool.available, ns)
		pool.mu.Unlock()
		return nil
	}
	pool.mu.Unlock()

	pool.mu.Lock()
	pool.total--
	pool.mu.Unlock()
	return ns.release()
}

func (c *cniProvider) New(ctx context.Context) (network.Namespace, error) {
	return c.nsPool.Get(ctx)
}

type cniNS struct {
	pool     *cniPool
	handle   cni.CNI
	id       string
	nativeID string
}

func (ns *cniNS) Set(s *specs.Spec) error {
	return setNetNS(s, ns.nativeID)
}

func (ns *cniNS) Close() error {
	return ns.pool.Put(ns)
}

func (ns *cniNS) release() error {
	err := ns.handle.Remove(context.TODO(), ns.id, ns.nativeID)
	if err1 := unmountNetNS(ns.nativeID); err1 != nil && err == nil {
		err = err1
	}
	if err1 := deleteNetNS(ns.nativeID); err1 != nil && err == nil {
		err = err1
	}
	return err
}
