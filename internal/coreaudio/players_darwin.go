package coreaudio

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"unsafe"
)

const (
	propProcessList   = 0x70727323 // 'prs#'
	propProcessPID    = 0x70706964 // 'ppid'
	propRunningOutput = 0x7069726f // 'piro'
	propIsRunning     = 0x70697221 // 'pir?'
	propDevices       = 0x70647623 // 'pdv#'
)

var (
	audioObjectGetPropertyDataSize    func(obj uint32, addr *propertyAddress, qualSize uint32, qual unsafe.Pointer, size *uint32) int32
	audioObjectAddPropertyListener    func(obj uint32, addr *propertyAddress, proc uintptr, client unsafe.Pointer) int32
	audioObjectRemovePropertyListener func(obj uint32, addr *propertyAddress, proc uintptr, client unsafe.Pointer) int32

	listenerPtr uintptr
	// watching is the players that property changes wake. Like the IOProc,
	// one callback serves them all.
	watching atomic.Pointer[players]

	// Core Audio never reports changes of propRunningOutput, but these change
	// with it: whether the process runs any IO, and on which output devices.
	processChanges = []propertyAddress{
		{propIsRunning, scopeGlobal, elementMain},
		{propDevices, scopeOutput, elementMain},
	}
)

// players follows whether any process other than this one plays sound.
// Core Audio counts a process as playing while it runs output, silent or
// not; such a process keeps the Mac awake by itself, so capturing then costs
// the Mac no sleep. Capturing while nothing plays would keep it awake.
type players struct {
	changed chan struct{}
	self    int32
	procs   map[uint32]int32 // process objects with listeners, and their PIDs
}

func watchPlayers() (*players, error) {
	p := &players{changed: make(chan struct{}, 1), self: int32(os.Getpid()), procs: map[uint32]int32{}}
	watching.Store(p)
	addr := propertyAddress{propProcessList, scopeGlobal, elementMain}
	if st := audioObjectAddPropertyListener(systemObject, &addr, listenerPtr, nil); st != 0 {
		watching.CompareAndSwap(p, nil)
		return nil, fmt.Errorf("watching which processes play sound: %v", osStatus(st))
	}
	return p, nil
}

// changes receives after a process starts, ends, or starts or stops
// playing. Without a watch, it never does.
func (p *players) changes() <-chan struct{} {
	if p == nil {
		return nil
	}
	return p.changed
}

func (p *players) close() {
	if p == nil {
		return
	}
	addr := propertyAddress{propProcessList, scopeGlobal, elementMain}
	audioObjectRemovePropertyListener(systemObject, &addr, listenerPtr, nil)
	for obj := range p.procs {
		for _, addr := range processChanges {
			// Fails for processes that have exited, whose listeners went
			// with them.
			audioObjectRemovePropertyListener(obj, &addr, listenerPtr, nil)
		}
	}
	watching.CompareAndSwap(p, nil)
}

// playing reports whether a process other than this one plays sound. It
// also starts listening to processes that are new since the last call.
// Without a watch, everything counts as playing.
func (p *players) playing() (bool, error) {
	if p == nil {
		return true, nil
	}
	objs, err := processes()
	if err != nil {
		return false, err
	}
	alive := make(map[uint32]bool, len(objs))
	playing := false
	for _, obj := range objs {
		alive[obj] = true
		pid, ok := p.procs[obj]
		if !ok {
			pid = -1
			_ = getProperty(obj, propProcessPID, nil, 0, unsafe.Pointer(&pid), 4)
			// Listen before reading, so that no change goes unnoticed.
			for _, addr := range processChanges {
				audioObjectAddPropertyListener(obj, &addr, listenerPtr, nil)
			}
			p.procs[obj] = pid
		}
		var running uint32
		if pid != p.self && getProperty(obj, propRunningOutput, nil, 0, unsafe.Pointer(&running), 4) == nil && running != 0 {
			playing = true
		}
	}
	for obj := range p.procs {
		if !alive[obj] {
			delete(p.procs, obj)
		}
	}
	return playing, nil
}

// wait returns true once another process plays sound, and false if ctx is
// done first.
func (p *players) wait(ctx context.Context) bool {
	for {
		if playing, err := p.playing(); playing || err != nil {
			return true // capture as if it played, rather than hear nothing
		}
		select {
		case <-ctx.Done():
			return false
		case <-p.changes():
		}
	}
}

// processes lists the process objects of everything that uses Core Audio.
func processes() ([]uint32, error) {
	addr := propertyAddress{propProcessList, scopeGlobal, elementMain}
	var size uint32
	if st := audioObjectGetPropertyDataSize(systemObject, &addr, 0, nil, &size); st != 0 {
		return nil, fmt.Errorf("listing processes: %v", osStatus(st))
	}
	// Room for a few more that start meanwhile.
	objs := make([]uint32, size/4+16)
	size = uint32(len(objs) * 4)
	if st := audioObjectGetPropertyData(systemObject, &addr, 0, nil, &size, unsafe.Pointer(&objs[0])); st != 0 {
		return nil, fmt.Errorf("listing processes: %v", osStatus(st))
	}
	return objs[:size/4], nil
}

// listener runs on a Core Audio notification thread whenever a process
// starts or ends, or starts or stops IO.
func listener(_, _ uint32, _, _ unsafe.Pointer) int32 {
	if p := watching.Load(); p != nil {
		select {
		case p.changed <- struct{}{}:
		default: // a change is already pending
		}
	}
	return 0
}
