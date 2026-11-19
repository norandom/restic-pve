// Package vma reads and writes Proxmox VE VMA v1 backup streams
// (uncompressed, as written by `vzdump --stdout`).
//
// Layout (all integers big-endian unless noted):
//
//	header (HeaderFixedSize bytes, then the blob buffer, padded to 512):
//	  0  magic "VMA\0"         4  version u32 (1)
//	  8  uuid [16]             24 ctime i64 (Unix seconds)
//	  32 md5 [16] over the whole header (header_size bytes) with this field zeroed
//	  48 blob_buffer_offset u32 (= HeaderFixedSize)
//	  52 blob_buffer_size u32  (bytes used in the blob buffer, incl. the unused byte 0)
//	  56 header_size u32       (HeaderFixedSize + blob buffer rounded up to 512)
//	  2044 config_names[256] u32 blob pointers (name, NUL-terminated)
//	  3068 config_data[256]  u32 blob pointers (data, verbatim)
//	  4096 dev_info[256] × 32 bytes, indexed by device id:
//	       devname_ptr u32, reserved u32, size u64, reserved [16]
//	blob: u16 LITTLE-endian length, then data; the first blob starts at position 1
//
//	extent (ExtentHeaderSize bytes, then block_count × BlockSize data bytes):
//	  0 magic "VMAE"  4 reserved u16 (0)  6 block_count u16
//	  8 uuid [16] (= header uuid)  24 md5 [16] over the 512-byte header with this field zeroed
//	  40 blockinfo[59] u64: mask (63–48) | reserved (47–40) | dev (39–32) | cluster (31–0)
//	  One BlockSize data block follows per set mask bit (bit i = 4 KiB block i of the
//	  cluster). A cluster with mask 0 is all zeros. Unused blockinfo slots are 0.
//
// The stream has no trailer: it ends after the last extent.
package vma

const (
	// ClusterSize is the unit of a blockinfo entry.
	ClusterSize = 65536
	// BlockSize is the size of one data block inside a cluster (one mask bit).
	BlockSize = 4096
	// BlocksPerCluster is the number of mask bits per cluster.
	BlocksPerCluster = ClusterSize / BlockSize

	// HeaderFixedSize is the size of the fixed header part before the blob buffer.
	HeaderFixedSize = 12288
	// ExtentHeaderSize is the size of an extent header.
	ExtentHeaderSize = 512
	// MaxExtentClusters is the number of blockinfo slots per extent.
	MaxExtentClusters = 59
	// WriterExtentClusters is the number of clusters this writer packs into
	// one extent. It matches the pve vma-writer, which flushes at
	// VMA_BLOCKS_PER_EXTENT - 1 entries; readers accept up to MaxExtentClusters.
	WriterExtentClusters = MaxExtentClusters - 1

	// Version is the only supported format version.
	Version = 1

	// MaxBlobSize is the largest blob payload (u16 length).
	MaxBlobSize = 65535
	// MaxConfigs is the number of config slots in the header.
	MaxConfigs = 256
	// MinDevID and MaxDevID bound valid device ids (0 is unused, 255 reserved).
	MinDevID = 1
	MaxDevID = 254

	// Header field offsets.
	OffMagic            = 0
	OffVersion          = 4
	OffUUID             = 8
	OffCTime            = 24
	OffMD5              = 32
	OffBlobBufferOffset = 48
	OffBlobBufferSize   = 52
	OffHeaderSize       = 56
	OffConfigNames      = 2044
	OffConfigData       = 3068
	OffDevInfo          = 4096
	DevInfoSize         = 32

	// Extent header field offsets.
	OffExtMagic      = 0
	OffExtReserved   = 4
	OffExtBlockCount = 6
	OffExtUUID       = 8
	OffExtMD5        = 24
	OffExtBlockInfo  = 40
)

var (
	// HeaderMagic starts every VMA stream.
	HeaderMagic = [4]byte{'V', 'M', 'A', 0}
	// ExtentMagic starts every extent.
	ExtentMagic = [4]byte{'V', 'M', 'A', 'E'}
)

// DevInfo describes one device of the stream.
type DevInfo struct {
	ID   uint8
	Name string
	Size uint64
}

// ConfigBlob is one configuration file carried in the header, verbatim
// (including "#qmdump#map:" lines).
type ConfigBlob struct {
	Name string
	Data []byte
}

// Header is the decoded stream header.
type Header struct {
	UUID    [16]byte
	CTime   int64
	Devices []DevInfo // ascending ID, as in the header's dev_info table
	Configs []ConfigBlob
}

// Clusters returns the number of clusters of a device of the given size.
func Clusters(size uint64) uint64 {
	return (size + ClusterSize - 1) / ClusterSize
}

// BlockInfo packs one extent blockinfo entry.
func BlockInfo(mask uint16, dev uint8, cluster uint32) uint64 {
	return uint64(mask)<<48 | uint64(dev)<<32 | uint64(cluster)
}
