// Code generated by bpf2go; DO NOT EDIT.
//go:build 386 || amd64 || amd64p32 || arm || arm64 || mips64le || mips64p32le || mipsle || ppc64le || riscv64
// +build 386 amd64 amd64p32 arm arm64 mips64le mips64p32le mipsle ppc64le riscv64

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"

	"github.com/cilium/ebpf"
)

type bpfL7Event struct {
	Fd       uint64
	Pid      uint32
	Status   uint32
	Duration uint64
	Protocol uint8
	Method   uint8
	Padding  uint16
	Payload  [512]int8
	_        [4]byte
}

type bpfL7Request struct {
	WriteTimeNs uint64
	Protocol    uint8
	Partial     uint8
	RequestType uint8
	_           [1]byte
	RequestId   int32
	Payload     [512]int8
}

// loadBpf returns the embedded CollectionSpec for bpf.
func loadBpf() (*ebpf.CollectionSpec, error) {
	reader := bytes.NewReader(_BpfBytes)
	spec, err := ebpf.LoadCollectionSpecFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("can't load bpf: %w", err)
	}

	return spec, err
}

// loadBpfObjects loads bpf and converts it into a struct.
//
// The following types are suitable as obj argument:
//
//	*bpfObjects
//	*bpfPrograms
//	*bpfMaps
//
// See ebpf.CollectionSpec.LoadAndAssign documentation for details.
func loadBpfObjects(obj interface{}, opts *ebpf.CollectionOptions) error {
	spec, err := loadBpf()
	if err != nil {
		return err
	}

	return spec.LoadAndAssign(obj, opts)
}

// bpfSpecs contains maps and programs before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type bpfSpecs struct {
	bpfProgramSpecs
	bpfMapSpecs
}

// bpfSpecs contains programs before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type bpfProgramSpecs struct {
	SysEnterRead  *ebpf.ProgramSpec `ebpf:"sys_enter_read"`
	SysEnterWrite *ebpf.ProgramSpec `ebpf:"sys_enter_write"`
}

// bpfMapSpecs contains maps before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type bpfMapSpecs struct {
	ActiveL7Requests *ebpf.MapSpec `ebpf:"active_l7_requests"`
	ActiveReads      *ebpf.MapSpec `ebpf:"active_reads"`
	L7EventHeap      *ebpf.MapSpec `ebpf:"l7_event_heap"`
	L7Events         *ebpf.MapSpec `ebpf:"l7_events"`
	L7RequestHeap    *ebpf.MapSpec `ebpf:"l7_request_heap"`
}

// bpfObjects contains all objects after they have been loaded into the kernel.
//
// It can be passed to loadBpfObjects or ebpf.CollectionSpec.LoadAndAssign.
type bpfObjects struct {
	bpfPrograms
	bpfMaps
}

func (o *bpfObjects) Close() error {
	return _BpfClose(
		&o.bpfPrograms,
		&o.bpfMaps,
	)
}

// bpfMaps contains all maps after they have been loaded into the kernel.
//
// It can be passed to loadBpfObjects or ebpf.CollectionSpec.LoadAndAssign.
type bpfMaps struct {
	ActiveL7Requests *ebpf.Map `ebpf:"active_l7_requests"`
	ActiveReads      *ebpf.Map `ebpf:"active_reads"`
	L7EventHeap      *ebpf.Map `ebpf:"l7_event_heap"`
	L7Events         *ebpf.Map `ebpf:"l7_events"`
	L7RequestHeap    *ebpf.Map `ebpf:"l7_request_heap"`
}

func (m *bpfMaps) Close() error {
	return _BpfClose(
		m.ActiveL7Requests,
		m.ActiveReads,
		m.L7EventHeap,
		m.L7Events,
		m.L7RequestHeap,
	)
}

// bpfPrograms contains all programs after they have been loaded into the kernel.
//
// It can be passed to loadBpfObjects or ebpf.CollectionSpec.LoadAndAssign.
type bpfPrograms struct {
	SysEnterRead  *ebpf.Program `ebpf:"sys_enter_read"`
	SysEnterWrite *ebpf.Program `ebpf:"sys_enter_write"`
}

func (p *bpfPrograms) Close() error {
	return _BpfClose(
		p.SysEnterRead,
		p.SysEnterWrite,
	)
}

func _BpfClose(closers ...io.Closer) error {
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Do not access this directly.
//
//go:embed bpf_bpfel.o
var _BpfBytes []byte
