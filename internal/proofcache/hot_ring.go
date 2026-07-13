package proofcache

import "sync/atomic"

const hotReplayRingSize = 256

type hotReplayRing struct {
	write atomic.Uint64
	slots [hotReplayRingSize]hotReplayRingSlot
}

type hotReplayRingSlot struct {
	sequence          atomic.Uint64
	nativeWallMS      int64
	materializeMicros int64
	stdoutBytes       int64
	stderrBytes       int64
}

func (r *hotReplayRing) Record(nativeWallMS int64, materializeMS float64, stdoutBytes, stderrBytes int) {
	seq := r.write.Add(1)
	slot := &r.slots[(seq-1)&(hotReplayRingSize-1)]
	slot.nativeWallMS = nativeWallMS
	slot.materializeMicros = int64(materializeMS * 1000)
	slot.stdoutBytes = int64(stdoutBytes)
	slot.stderrBytes = int64(stderrBytes)
	slot.sequence.Store(seq)
}
