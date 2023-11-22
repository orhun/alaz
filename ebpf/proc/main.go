package proc

import (
	"context"
	"time"
	"unsafe"

	"github.com/ddosify/alaz/log"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

const (
	BPF_EVENT_PROC_EXEC = iota + 1
	BPF_EVENT_PROC_EXIT
)

const (
	EVENT_PROC_EXEC = "EVENT_PROC_EXEC"
	EVENT_PROC_EXIT = "EVENT_PROC_EXIT"
)

// Custom type for the enumeration
type ProcEventConversion uint32

// String representation of the enumeration values
func (e ProcEventConversion) String() string {
	switch e {
	case BPF_EVENT_PROC_EXEC:
		return EVENT_PROC_EXEC
	case BPF_EVENT_PROC_EXIT:
		return EVENT_PROC_EXIT
	default:
		return "Unknown"
	}
}

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS bpf proc.c -- -I../headers

// for both ebpf and userspace
type PEvent struct {
	Pid   uint32
	Type_ uint8
}

type ProcEvent struct {
	Pid   uint32
	Type_ string
}

const PROC_EVENT = "proc_event"

func (e PEvent) Type() string {
	return PROC_EVENT
}

var objs bpfObjects

func LoadBpfObjects() {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Logger.Fatal().Err(err).Msg("failed to remove memlock limit")
	}

	// Load pre-compiled programs and maps into the kernel.
	objs = bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		log.Logger.Fatal().Err(err).Msg("loading objects")
	}
}

// returns when program is detached
func DeployAndWait(parentCtx context.Context, ch chan interface{}) {
	ctx, _ := context.WithCancel(parentCtx)
	defer objs.Close()

	time.Sleep(1 * time.Second)

	l, err := link.Tracepoint("sched", "sched_process_exit", objs.bpfPrograms.SchedProcessExit, nil)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("link sched_process_exit tracepoint")
	}
	defer func() {
		log.Logger.Info().Msg("closing sched_process_exit tracepoint")
		l.Close()
	}()

	l1, err := link.Tracepoint("sched", "sched_process_exec", objs.bpfPrograms.SchedProcessExec, nil)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("link sched_process_exec tracepoint")
	}
	defer func() {
		log.Logger.Info().Msg("closing sched_process_exec tracepoint")
		l1.Close()
	}()

	pEvents, err := ringbuf.NewReader(objs.Rb)
	if err != nil {
		log.Logger.Fatal().Err(err).Msg("error creating ringbuf reader")
	}
	defer func() {
		log.Logger.Info().Msg("closing pExitEvents ringbuf reader")
		pEvents.Close()
	}()

	// go listenDebugMsgs()

	readDone := make(chan struct{})
	go func() {
		for {
			read := func() {
				record, err := pEvents.Read()
				if err != nil {
					log.Logger.Warn().Err(err).Msg("error reading from pExitEvents")
				}

				bpfEvent := (*PEvent)(unsafe.Pointer(&record.RawSample[0]))

				go func() {
					log.Logger.Info().Msgf("pid: %d, type: %s", bpfEvent.Pid, ProcEventConversion(bpfEvent.Type_).String())

					ch <- ProcEvent{
						Pid:   bpfEvent.Pid,
						Type_: ProcEventConversion(bpfEvent.Type_).String(),
					}
				}()
			}

			select {
			case <-readDone:
				return
			default:
				read()
			}

		}
	}()

	<-ctx.Done() // wait for context to be cancelled
	readDone <- struct{}{}
	// defers will clean up
}
