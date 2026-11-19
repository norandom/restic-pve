// Package verify computes the per-device sha256ct fingerprint of a guest
// image and reads, writes and compares the manifest JSON (schema v1).
//
// sha256ct (Requirements 10.2, 10.3): with h_i = SHA-256 of cluster i (the
// final cluster only at its real length),
//
//	sha256ct = SHA-256("RVS1-CT\x00" ‖ u64le(size) ‖ h_0 ‖ … ‖ h_{n-1})
//
// This SHA-256 over device content can be built from precomputed cluster
// hashes. A backup can fingerprint patched (evicted) clusters without
// holding their data.
package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"

	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// DomainPrefix starts the sha256ct outer hash.
const DomainPrefix = "RVS1-CT\x00"

const clusterSize = vma.ClusterSize

var (
	zeroCluster     = make([]byte, clusterSize)
	zeroClusterHash = sha256.Sum256(zeroCluster)
)

// Digest is the streaming sha256ct of one device. Clusters are added in
// ascending index, each either as data (Cluster) or as a precomputed hash
// (ClusterHash). Out-of-order use is a programming error and panics.
type Digest struct {
	size     uint64
	clusters uint64
	next     uint64
	outer    hash.Hash
	zero     uint64
	alloc    uint64
}

// NewDigest starts the fingerprint of a device of size bytes.
func NewDigest(size uint64) *Digest {
	d := &Digest{size: size, clusters: vma.Clusters(size), outer: sha256.New()}
	d.outer.Write([]byte(DomainPrefix))
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], size)
	d.outer.Write(b[:])
	return d
}

// realLen checks that index is the next cluster and returns its real length.
func (d *Digest) realLen(index uint32) uint64 {
	if uint64(index) != d.next || d.next >= d.clusters {
		panic(fmt.Sprintf("verify: cluster %d out of order (next %d of %d)", index, d.next, d.clusters))
	}
	return min(clusterSize, d.size-uint64(index)*clusterSize)
}

// Cluster adds cluster index from its bytes. data is nil for a zero cluster,
// or at least the real length (ClusterSize, or exactly the real length of
// the final cluster); only the real length is hashed. An all-zero cluster is
// counted in ZeroClusters, any other in AllocatedBytes.
func (d *Digest) Cluster(index uint32, data []byte) {
	n := d.realLen(index)
	var sum [32]byte
	switch {
	case data != nil && uint64(len(data)) < n:
		panic(fmt.Sprintf("verify: cluster %d data %d bytes, need %d", index, len(data), n))
	case data == nil || bytes.Equal(data[:n], zeroCluster[:n]):
		d.zero++
		if n == clusterSize {
			sum = zeroClusterHash
		} else {
			sum = sha256.Sum256(zeroCluster[:n])
		}
	default:
		d.alloc += n
		sum = sha256.Sum256(data[:n])
	}
	d.outer.Write(sum[:])
	d.next++
}

// ClusterHash adds cluster index from its precomputed SHA-256 (over the real
// length), as for a patched cluster. It counts as allocated.
func (d *Digest) ClusterHash(index uint32, sum [32]byte) {
	d.alloc += d.realLen(index)
	d.outer.Write(sum[:])
	d.next++
}

// Complete reports whether every cluster has been added.
func (d *Digest) Complete() bool { return d.next == d.clusters }

// Size is the device size the digest binds.
func (d *Digest) Size() uint64 { return d.size }

// ZeroClusters is the number of all-zero clusters added as data.
func (d *Digest) ZeroClusters() uint64 { return d.zero }

// AllocatedBytes is the real length of every non-zero cluster added.
func (d *Digest) AllocatedBytes() uint64 { return d.alloc }

// Sum returns sha256ct. It panics if clusters are missing.
func (d *Digest) Sum() [32]byte {
	if !d.Complete() {
		panic(fmt.Sprintf("verify: Sum with %d of %d clusters", d.next, d.clusters))
	}
	var out [32]byte
	copy(out[:], d.outer.Sum(nil))
	return out
}

// Hex returns the lowercase hex form used in the trailer and manifest.
func Hex(sum [32]byte) string { return hex.EncodeToString(sum[:]) }

var (
	_ reorder.Sink     = (*Fingerprint)(nil)
	_ reorder.HashSink = (*Fingerprint)(nil)
)

// Fingerprint computes sha256ct for every device of a layout. It is a
// reorder.Sink and reorder.HashSink: InOrder data is hashed in cursor order,
// Patch is ignored (its hash arrives through ClusterHash), and ClusterHash
// must come immediately before the placeholder InOrder(dev, index, nil) of a
// patched position. Contract violations are returned (sticky) by InOrder.
type Fingerprint struct {
	devs    []vma.DevInfo
	digests []*Digest
	cur     int
	hashed  *pendingHash
	err     error
}

type pendingHash struct {
	dev   uint8
	index uint32
	sum   [32]byte
}

// NewFingerprint starts the fingerprints of every device of l.
func NewFingerprint(l reorder.Layout) *Fingerprint {
	f := &Fingerprint{devs: append([]vma.DevInfo(nil), l.Devices...)}
	for _, d := range f.devs {
		f.digests = append(f.digests, NewDigest(d.Size))
	}
	f.skipComplete()
	return f
}

func (f *Fingerprint) skipComplete() {
	for f.cur < len(f.digests) && f.digests[f.cur].Complete() {
		f.cur++
	}
}

func (f *Fingerprint) fail(format string, a ...any) error {
	if f.err == nil {
		f.err = fmt.Errorf("verify: "+format, a...)
	}
	return f.err
}

// InOrder adds the next cluster at the cursor.
func (f *Fingerprint) InOrder(dev uint8, index uint32, data []byte) error {
	if f.err != nil {
		return f.err
	}
	if f.cur >= len(f.digests) {
		return f.fail("InOrder(%d,%d) after the last cluster", dev, index)
	}
	d := f.digests[f.cur]
	if dev != f.devs[f.cur].ID || uint64(index) != d.next {
		return f.fail("InOrder(%d,%d), cursor at (%d,%d)", dev, index, f.devs[f.cur].ID, d.next)
	}
	if n := min(clusterSize, d.size-uint64(index)*clusterSize); data != nil && uint64(len(data)) < n {
		return f.fail("InOrder(%d,%d) data %d bytes, need %d", dev, index, len(data), n)
	}
	if p := f.hashed; p != nil {
		f.hashed = nil
		if p.dev != dev || p.index != index {
			return f.fail("ClusterHash(%d,%d) not followed by its InOrder (got %d,%d)", p.dev, p.index, dev, index)
		}
		if data != nil {
			return f.fail("InOrder(%d,%d) carries data for a patched position", dev, index)
		}
		d.ClusterHash(index, p.sum)
	} else {
		d.Cluster(index, data)
	}
	f.skipComplete()
	return nil
}

// Patch is a no-op: the fingerprint takes a patched cluster's hash from
// ClusterHash when the cursor reaches it.
func (f *Fingerprint) Patch(uint8, uint32, []byte) error { return f.err }

// ClusterHash announces the hash of the patched position the cursor is at.
func (f *Fingerprint) ClusterHash(dev uint8, index uint32, sum [32]byte) {
	if f.err != nil {
		return
	}
	if f.hashed != nil {
		f.fail("ClusterHash(%d,%d) while (%d,%d) is pending", dev, index, f.hashed.dev, f.hashed.index)
		return
	}
	f.hashed = &pendingHash{dev, index, sum}
}

// Err returns the first contract violation, if any.
func (f *Fingerprint) Err() error { return f.err }

// Devices returns the trailer entries (id, name, size, sha256ct,
// allocated_bytes, zero_clusters) once every cluster has been added.
func (f *Fingerprint) Devices() ([]rvs.TrailerDevice, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.cur < len(f.digests) || f.hashed != nil {
		return nil, fmt.Errorf("verify: fingerprint incomplete")
	}
	out := make([]rvs.TrailerDevice, len(f.devs))
	for i, d := range f.devs {
		g := f.digests[i]
		out[i] = rvs.TrailerDevice{
			ID: d.ID, Name: d.Name, Size: d.Size, SHA256CT: Hex(g.Sum()),
			AllocatedBytes: g.AllocatedBytes(), ZeroClusters: g.ZeroClusters(),
		}
	}
	return out, nil
}

// Check compares the computed digests with a stream's trailer. A device set,
// size or digest difference is an integrity error (rvs.ErrIntegrity, exit 3).
func (f *Fingerprint) Check(t *rvs.Trailer) error {
	got, err := f.Devices()
	if err != nil {
		return err
	}
	if len(t.Devices) != len(got) {
		return fmt.Errorf("%w: trailer lists %d devices, stream has %d", rvs.ErrIntegrity, len(t.Devices), len(got))
	}
	for i, g := range got {
		w := t.Devices[i]
		if w.ID != g.ID || w.Name != g.Name || w.Size != g.Size {
			return fmt.Errorf("%w: trailer device %d (%q, %d bytes) differs from stream device %d (%q, %d bytes)",
				rvs.ErrIntegrity, w.ID, w.Name, w.Size, g.ID, g.Name, g.Size)
		}
		if !digestEqual(w.SHA256CT, g.SHA256CT) {
			return fmt.Errorf("%w: device %d (%s) sha256ct %s, trailer says %s", rvs.ErrIntegrity, g.ID, g.Name, g.SHA256CT, w.SHA256CT)
		}
	}
	return nil
}
