// Command fixturegen writes the synthetic VMA streams of the integration
// suite (neutral guest "example", VMID 100) and their expected device
// digests. It is test tooling only and never part of the vmastream binary.
//
//	fixturegen -out DIR [-big-mib 512] [-shuffled-mib 160]
//
// Files written to DIR:
//
//	example.vma           two devices (drive-scsi0, drive-efidisk0 with a
//	                      partial last cluster), interleaved arrival order,
//	                      built with vmatest.Builder
//	example-tampered.vma  the same stream with a flipped extent md5
//	example-shuffled.vma  a larger data disk in random arrival order (forces
//	                      the reorder buffer's capacity error at small caps)
//	example-big.vma       a large data disk in plain order (interrupt tests)
//	expected.json         per file: device name, size and SHA-256 of the
//	                      device contents, SHA-256 of each config blob
//
// The large streams are written cluster by cluster through vma.Writer.
// Each cluster has deterministic content. Memory use stays constant. Output is
// deterministic (fixed uuid, ctime and seeds).
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// Fixed identity of every generated stream (reproducible output).
var (
	fixtureUUID  = [16]byte{0x0e, 0x8a, 0x3b, 0x1f, 0, 0, 0x40, 0, 0x80, 0, 0, 0, 0, 0, 1, 0}
	fixtureCTime = int64(1760140800)
)

const (
	efiSize   = 540672 // 528 KiB: 8 full clusters and a partial one
	smallScsi = 16*1024*1024 + 12345
)

// DeviceDigest is the expected content of one device.
type DeviceDigest struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
}

// Expected describes one generated stream.
type Expected struct {
	Devices []DeviceDigest    `json:"devices"`
	Configs map[string]string `json:"configs"` // name -> SHA-256 of the blob
	Bytes   int64             `json:"bytes"`   // stream length
}

// Config is the neutral qemu-server.conf carried by every stream.
func Config() vma.ConfigBlob {
	return vma.ConfigBlob{Name: "qemu-server.conf", Data: []byte(`#qmdump#map:efidisk0:drive-efidisk0:fast-ssd:raw:
#qmdump#map:scsi0:drive-scsi0:fast-ssd:raw:
agent: 1
bios: ovmf
boot: order=scsi0
cores: 2
efidisk0: fast-ssd:vm-100-disk-1,efitype=4m,pre-enrolled-keys=1,size=528K
memory: 2048
name: example-vm
net0: virtio=02:00:00:00:01:00,bridge=vmbr0
ostype: l26
scsi0: fast-ssd:vm-100-disk-0,discard=on,iothread=1,size=32G
scsihw: virtio-scsi-single
`)}
}

func configDigests() map[string]string {
	c := Config()
	sum := sha256.Sum256(c.Data)
	return map[string]string{c.Name: hex.EncodeToString(sum[:])}
}

// Generate writes every fixture to dir and returns the expectations.
func Generate(dir string, bigMiB, shuffledMiB uint64) (map[string]Expected, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := map[string]Expected{}
	small := func(t vmatest.Tamper) *vmatest.Builder {
		devs := []vmatest.Device{
			vmatest.DataDisk(1, "scsi0", smallScsi, 101),
			vmatest.EFIDisk(2, efiSize, 102),
		}
		return &vmatest.Builder{UUID: fixtureUUID, CTime: fixtureCTime, Devices: devs,
			Configs: []vma.ConfigBlob{Config()}, Order: vmatest.InterleavedOrder(devs), Tamper: t}
	}
	for name, b := range map[string]*vmatest.Builder{
		"example.vma": small(vmatest.Tamper{}),
		// a middle extent: the reader fails after some clusters were accepted
		"example-tampered.vma": small(vmatest.Tamper{Kind: vmatest.TamperExtentMD5, Extent: 2}),
	} {
		r, err := b.Build()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), r.Stream, 0o644); err != nil {
			return nil, err
		}
		exp := Expected{Configs: configDigests(), Bytes: int64(len(r.Stream))}
		for _, d := range b.Devices {
			sum := sha256.Sum256(b.Contents(d.ID))
			exp.Devices = append(exp.Devices, DeviceDigest{d.Name, d.Size, hex.EncodeToString(sum[:])})
		}
		out[name] = exp
	}
	streams := []struct {
		name     string
		scsiSize uint64
		seed     int64
		shuffle  bool
	}{
		{"example-shuffled.vma", shuffledMiB << 20, 201, true},
		{"example-big.vma", bigMiB << 20, 301, false},
	}
	for _, s := range streams {
		devs := []StreamDevice{{ID: 1, Name: "drive-scsi0", Size: s.scsiSize, Seed: s.seed},
			{ID: 2, Name: "drive-efidisk0", Size: efiSize, Seed: s.seed + 1}}
		exp, err := writeStream(filepath.Join(dir, s.name), devs, s.shuffle, s.seed)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		out[s.name] = exp
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return out, os.WriteFile(filepath.Join(dir, "expected.json"), append(data, '\n'), 0o644)
}

// StreamDevice is a device whose clusters are generated on demand.
type StreamDevice struct {
	ID   uint8
	Name string
	Size uint64
	Seed int64
}

// ClusterData returns the content of cluster i of d (the tail length for the
// last cluster): random data where every fifth cluster (from 1) is all zeros
// and every fifth cluster (from 3) has its even 4 KiB blocks zeroed, as
// vmatest.PatternData, but seeded per cluster so any order is cheap.
func ClusterData(d StreamDevice, i uint64) []byte {
	lo := i * vma.ClusterSize
	n := min(uint64(vma.ClusterSize), d.Size-lo)
	b := make([]byte, n)
	if i%5 == 1 {
		return b
	}
	rand.New(rand.NewSource(d.Seed<<40 ^ int64(i))).Read(b)
	if i%5 == 3 {
		for blk := uint64(0); blk < n; blk += 2 * vma.BlockSize {
			clear(b[blk:min(blk+vma.BlockSize, n)])
		}
	}
	return b
}

func writeStream(path string, devs []StreamDevice, shuffle bool, seed int64) (Expected, error) {
	f, err := os.Create(path)
	if err != nil {
		return Expected{}, err
	}
	cw := &countWriter{w: bufio.NewWriterSize(f, 1<<20)}
	exp, werr := streamTo(cw, devs, shuffle, seed)
	if werr == nil {
		werr = cw.w.(*bufio.Writer).Flush()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	exp.Bytes = cw.n
	return exp, werr
}

func streamTo(w io.Writer, devs []StreamDevice, shuffle bool, seed int64) (Expected, error) {
	infos := make([]vma.DevInfo, 0, len(devs))
	vdevs := make([]vmatest.Device, 0, len(devs)) // sizes only, for the order helpers
	byID := map[uint8]StreamDevice{}
	for _, d := range devs {
		infos = append(infos, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
		vdevs = append(vdevs, vmatest.Device{ID: d.ID, Name: d.Name, Size: d.Size})
		byID[d.ID] = d
	}
	vw, err := vma.NewWriter(w, vma.WriterOptions{UUID: fixtureUUID, CTime: fixtureCTime, Devices: infos,
		Configs: []vma.ConfigBlob{Config()}})
	if err != nil {
		return Expected{}, err
	}
	order := vmatest.SequentialOrder(vdevs)
	if shuffle {
		order = vmatest.ShuffledOrder(vdevs, seed)
	}
	for _, r := range order {
		if err := vw.WriteCluster(r.Dev, r.Index, ClusterData(byID[r.Dev], uint64(r.Index))); err != nil {
			vw.Abort()
			return Expected{}, err
		}
	}
	if err := vw.Commit(); err != nil {
		return Expected{}, err
	}
	exp := Expected{Configs: configDigests()}
	for _, d := range devs {
		h := sha256.New()
		for i := uint64(0); i < vma.Clusters(d.Size); i++ {
			h.Write(ClusterData(d, i))
		}
		exp.Devices = append(exp.Devices, DeviceDigest{d.Name, d.Size, hex.EncodeToString(h.Sum(nil))})
	}
	return exp, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

func main() {
	fs := flag.NewFlagSet("fixturegen", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	big := fs.Uint64("big-mib", 512, "data disk size of example-big.vma in MiB")
	shuffled := fs.Uint64("shuffled-mib", 160, "data disk size of example-shuffled.vma in MiB")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if *out == "" || *big == 0 || *shuffled == 0 {
		fmt.Fprintln(os.Stderr, "fixturegen: -out DIR is required; sizes must be positive")
		os.Exit(2)
	}
	exp, err := Generate(*out, *big, *shuffled)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixturegen:", err)
		os.Exit(1)
	}
	for name, e := range exp {
		fmt.Printf("%s: %d bytes, %d devices\n", name, e.Bytes, len(e.Devices))
	}
}
