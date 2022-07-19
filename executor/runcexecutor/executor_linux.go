package runcexecutor

import (
	"context"
	"io"
	"os"
	"runtime"
	"syscall"

	"github.com/containerd/console"
	runc "github.com/containerd/go-runc"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/sys/signal"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

func updateRuncFieldsForHostOS(runtime *runc.Runc) {
	// PdeathSignal only supported on unix platforms
	runtime.PdeathSignal = syscall.SIGKILL // this can still leak the process
}

func (w *runcExecutor) run(ctx context.Context, id, bundle string, process executor.ProcessInfo, started func()) error {
	return w.callWithIO(ctx, id, bundle, process, started, func(ctx context.Context, started chan<- int, io runc.IO) error {
		// To prevent runc from receiving a SIGKILL from the OS thread exiting
		// See https://github.com/golang/go/issues/27505
		// TODO: move to go-runc package's (defaultMonitor).Start or methods which call it
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		_, err := w.runc.Run(ctx, id, bundle, &runc.CreateOpts{
			NoPivot: w.noPivot,
			Started: started,
			IO:      io,
		})
		if err != nil {
			bklog.G(ctx).Errorf("runc.Run returned error: %s", err)
		}
		return err
	})
}

func (w *runcExecutor) exec(ctx context.Context, id, bundle string, specsProcess *specs.Process, process executor.ProcessInfo, started func()) error {
	return w.callWithIO(ctx, id, bundle, process, started, func(ctx context.Context, started chan<- int, io runc.IO) error {
		// To prevent runc from receiving a SIGKILL from the OS thread exiting
		// See https://github.com/golang/go/issues/27505
		// TODO: move to go-runc package's (defaultMonitor).Start or methods which call it
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		return w.runc.Exec(ctx, id, *specsProcess, &runc.ExecOpts{
			Started: started,
			IO:      io,
		})
	})
}

type runcCall func(ctx context.Context, started chan<- int, io runc.IO) error

func (w *runcExecutor) callWithIO(ctx context.Context, id, bundle string, process executor.ProcessInfo, started func(), call runcCall) error {
	runcProcess := &startingProcess{
		ready: make(chan struct{}),
	}
	defer runcProcess.Release()

	var eg errgroup.Group
	egCtx, cancel := context.WithCancel(ctx)
	defer func() {
		bklog.G(ctx).Debugf("waiting for error group")
		if err := eg.Wait(); err != nil && !errors.Is(err, context.Canceled) {
			bklog.G(ctx).Warningf("error from runc error group: %s", err)
		}
		bklog.G(ctx).Debugf("done waiting for error group")
	}()
	defer cancel()

	startedCh := make(chan int, 1)
	eg.Go(func() error {
		defer bklog.G(ctx).Debugf("WaitForStart goroutine finished")
		return runcProcess.WaitForStart(egCtx, startedCh, started)
	})

	eg.Go(func() error {
		defer bklog.G(ctx).Debugf("handleSignals goroutine finished")
		return handleSignals(egCtx, runcProcess, process.Signal)
	})

	if !process.Meta.Tty {
		return call(ctx, startedCh, &forwardIO{stdin: process.Stdin, stdout: process.Stdout, stderr: process.Stderr})
	}

	ptm, ptsName, err := console.NewPty()
	if err != nil {
		return err
	}

	pts, err := os.OpenFile(ptsName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptm.Close()
		return err
	}

	defer func() {
		if process.Stdin != nil {
			process.Stdin.Close()
		}
		pts.Close()
		ptm.Close()
		cancel() // this will shutdown resize and signal loops
		bklog.G(ctx).Debugf("waiting for error group (with tty)")
		err := eg.Wait()
		if err != nil {
			bklog.G(ctx).Warningf("error while shutting down tty io: %s", err)
		}
		bklog.G(ctx).Debugf("done waiting for error group (with tty)")
	}()

	if process.Stdin != nil {
		eg.Go(func() error {
			defer bklog.G(ctx).Debugf("stdin goroutine finished")
			_, err := io.Copy(ptm, process.Stdin)
			// stdin might be a pipe, so this is like EOF
			if errors.Is(err, io.ErrClosedPipe) {
				return nil
			}
			return err
		})
	}

	if process.Stdout != nil {
		eg.Go(func() error {
			defer bklog.G(ctx).Debugf("stdout goroutine finished")
			_, err := io.Copy(process.Stdout, ptm)
			// ignore `read /dev/ptmx: input/output error` when ptm is closed
			var ptmClosedError *os.PathError
			if errors.As(err, &ptmClosedError) {
				if ptmClosedError.Op == "read" &&
					ptmClosedError.Path == "/dev/ptmx" &&
					ptmClosedError.Err == syscall.EIO {
					return nil
				}
			}
			return err
		})
	}

	eg.Go(func() error {
		defer bklog.G(ctx).Debugf("WaitForReady goroutine finished")
		err := runcProcess.WaitForReady(egCtx)
		if err != nil {
			return err
		}
		for {
			select {
			case <-egCtx.Done():
				return nil
			case resize := <-process.Resize:
				err = ptm.Resize(console.WinSize{
					Height: uint16(resize.Rows),
					Width:  uint16(resize.Cols),
				})
				if err != nil {
					bklog.G(ctx).Errorf("failed to resize ptm: %s", err)
				}
				err = runcProcess.Process.Signal(signal.SIGWINCH)
				if err != nil {
					bklog.G(ctx).Errorf("failed to send SIGWINCH to process: %s", err)
				}
			}
		}
	})

	runcIO := &forwardIO{}
	if process.Stdin != nil {
		runcIO.stdin = pts
	}
	if process.Stdout != nil {
		runcIO.stdout = pts
	}
	if process.Stderr != nil {
		runcIO.stderr = pts
	}

	return call(ctx, startedCh, runcIO)
}
