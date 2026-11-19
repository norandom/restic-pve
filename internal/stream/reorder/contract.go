// Package reorder puts VMA clusters in cursor order: devices in ascending
// id, then cluster index. vzdump emits clusters roughly in order, but
// copy-before-write writes arrive early.
//
// RAM use is bounded by Caps.RAMBytes. Early clusters are held in RAM first.
// When the RAM cap would be exceeded, the farthest cluster is written ahead
// through Sink.Patch and only its SHA-256 is kept (a HashSlotBytes hash slot).
// Early zero clusters cost one bit in a per-device bitmap. Stats.PatchCount
// counts patches; ErrCapacity occurs only when hash slots and bitmaps fill RAM.
package reorder

import (
	"errors"

	"github.com/norandom/restic-pve/internal/stream/vma"
)

// ClusterSize is the reorder unit (one VMA cluster, one RAM entry).
const ClusterSize = vma.ClusterSize

// HashSlotBytes is the RAM accounted for one patched cluster's hash slot.
const HashSlotBytes = 32

// Caps bounds the buffer. RAMBytes covers buffered cluster data, hash slots
// and the zero-cluster bitmaps.
type Caps struct {
	RAMBytes int64
}

// Layout is the fixed emission order: devices in ascending ID (the order of
// the VMA header's dev_info table).
type Layout struct {
	Devices []vma.DevInfo
}

// Sink receives the reassembled stream.
//
// Contract (relied on by the RVS1 writer and the fingerprint):
//   - InOrder is called exactly once per cluster of every device, in cursor
//     order (devices in Layout order, then ascending index). data is nil for a
//     zero cluster and for a position that was already supplied by Patch (the
//     placeholder); otherwise it is ClusterSize bytes.
//   - Patch is called for an early cluster that could not be buffered. It is
//     always called before InOrder reaches that position, at most once per
//     position, with ClusterSize bytes.
//   - For the final cluster of a device data is still ClusterSize bytes; the
//     consumer truncates it to the device size.
//   - data is only valid during the call (the buffer may reuse it).
type Sink interface {
	InOrder(dev uint8, index uint32, data []byte) error // next cluster at the cursor (nil data = zero cluster)
	Patch(dev uint8, index uint32, data []byte) error   // evicted early cluster, written ahead as patch frame
}

// HashSink receives the hash of every patched cluster when the cursor reaches
// its position: ClusterHash(dev, index, sum) is called immediately before the
// placeholder InOrder(dev, index, nil). sum is SHA-256 over the cluster bytes,
// truncated to the device size for the final cluster (the per-cluster hash of
// the sha256ct fingerprint).
type HashSink interface {
	ClusterHash(dev uint8, index uint32, sum [32]byte)
}

// Stats reports the buffer's peak RAM use and overflow counters, plus the
// current RAMInUse (same accounting as RAMPeak) for progress reporting.
type Stats struct {
	RAMPeak                   int64
	PatchCount, EarlyClusters uint64
	RAMInUse                  int64
}

var (
	// ErrMissingCluster is returned by Finish when clusters never arrived.
	ErrMissingCluster = errors.New("reorder: missing clusters")
	// ErrInvalidCluster rejects a cluster outside the layout, a duplicate, or
	// one with a wrong data length.
	ErrInvalidCluster = errors.New("reorder: invalid cluster")
	// ErrCapacity means RAMBytes cannot even hold the hash slot of a patch:
	// RAM is entirely taken by hash slots and bitmaps (RAMBytes/32 patched
	// clusters outstanding). It cannot occur when RAMBytes covers the bitmaps
	// plus HashSlotBytes per outstanding patch.
	ErrCapacity = errors.New("reorder: RAM cap exhausted by hash slots")
)
