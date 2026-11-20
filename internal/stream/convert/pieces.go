package convert

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// The pieces input is the tar `restic dump --archive tar <snapshot> <root>` writes for a
// snapshot in the piece layout: every file in restic's name order (pieces.Files), the
// manifest first, then the config files, then each disk's pieces; folders may come between
// them. One piece is in memory at a time.

// maxConfigBytes bounds one config file (a VMA config blob is far smaller).
const maxConfigBytes = 1 << 20

// tarInput remembers a read error of the input, so that a broken tar (integrity) and a
// failing input (local I/O) are told apart.
type tarInput struct {
	r   io.Reader
	err error
}

func (t *tarInput) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF && t.err == nil {
		t.err = err
	}
	return n, err
}

func hasMapLines(conf []byte) bool {
	return bytes.HasPrefix(conf, []byte("#qmdump#map:")) || bytes.Contains(conf, []byte("\n#qmdump#map:"))
}

// walkPieces reads a pieces tar and hands every cluster to h in device order. Each piece must
// come once, in order, with the manifest's SHA-256; each disk must give its sha256pt; config
// files must match their digests. With qmdump, a qemu-server.conf without "#qmdump#map:"
// lines (a provider config) gets the lines the manifest kept from the last full read, in
// front, as vzdump writes them.
func walkPieces(in io.Reader, h handler, o Options, qmdump bool) (*Result, error) {
	src := &tarInput{r: in}
	tr := tar.NewReader(src)
	broken := func(what string, err error) error {
		if src.err != nil {
			return localIO("read input", src.err)
		}
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return integrity("pieces tar: %s: %v", what, err)
	}
	var (
		m        *pieces.Manifest
		prefix   string
		want     []string
		next     int
		devs     []pieces.Device // in name order, as their pieces come
		dev      int
		piece    uint64
		pt       *pieces.PT
		hdr      *rvs.Header
		res      *Result
		buf      = make([]byte, pieces.PieceSize)
		vmaDevs  []vma.DevInfo
		confDigs = map[string]string{}
	)
	for {
		e, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, broken("entry header", err)
		}
		if e.Typeflag == tar.TypeDir {
			continue
		}
		name := strings.TrimPrefix(e.Name, "/")
		if e.Typeflag != tar.TypeReg {
			return nil, integrity("pieces tar: %s is not a plain file", name)
		}
		if m == nil {
			p, ok := strings.CutSuffix(name, pieces.ManifestName)
			if !ok || p != "" && !strings.HasSuffix(p, "/") {
				return nil, integrity("pieces tar: the first file is %s, not %s", name, pieces.ManifestName)
			}
			b, err := io.ReadAll(io.LimitReader(tr, pieces.MaxManifestBytes+1))
			if err != nil {
				return nil, broken(name, err)
			}
			if m, err = pieces.ReadManifest(bytes.NewReader(b)); err != nil {
				return nil, err
			}
			prefix, want, next = p, pieces.Files(m), 1
			devs = append(devs, m.Devices...)
			sort.Slice(devs, func(i, j int) bool { return devs[i].Name < devs[j].Name })
			pt = pieces.NewPT(devs[0].Size)
			hdr = &rvs.Header{}
			for _, d := range m.Devices {
				vmaDevs = append(vmaDevs, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
			}
			for _, c := range m.Config {
				confDigs[c.Name] = c.SHA256
			}
			continue
		}
		rel, ok := strings.CutPrefix(name, prefix)
		if !ok || next >= len(want) || rel != want[next] {
			should := "the end of the tar"
			if next < len(want) {
				should = want[next]
			}
			return nil, integrity("pieces tar: %s where %s should come", name, should)
		}
		next++
		if next <= 1+len(m.Config) { // config/<name>
			cname := strings.TrimPrefix(rel, pieces.ConfigDir+"/")
			if e.Size > maxConfigBytes {
				return nil, integrity("pieces tar: %s is %d bytes", name, e.Size)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, broken(name, err)
			}
			if pieces.PieceDigest(data) != confDigs[cname] {
				return nil, integrity("pieces tar: %s does not match its manifest digest", cname)
			}
			if qmdump && cname == "qemu-server.conf" && len(m.QmdumpMap) > 0 && !hasMapLines(data) {
				data = append([]byte(strings.Join(m.QmdumpMap, "\n")+"\n"), data...)
			}
			hdr.Configs = append(hdr.Configs, vma.ConfigBlob{Name: cname, Data: data})
			continue
		}
		if res == nil {
			hdr.Devices = vmaDevs
			if err := h.begin(hdr); err != nil {
				return nil, err
			}
			res = &Result{Header: hdr}
		}
		d := &devs[dev]
		n := pieces.PieceLen(d.Size, piece)
		if uint64(e.Size) != n {
			return nil, integrity("pieces tar: %s piece %d is %d bytes, want %d", d.Name, piece, e.Size, n)
		}
		if _, err := io.ReadFull(tr, buf[:n]); err != nil {
			return nil, broken(name, err)
		}
		if got := pieces.PieceDigest(buf[:n]); got != d.Digests[piece] {
			return nil, integrity("pieces tar: %s piece %d: SHA-256 %s, manifest %s", d.Name, piece, got, d.Digests[piece])
		}
		if err := pt.AddData(buf[:n]); err != nil {
			return nil, integrity("pieces tar: %v", err)
		}
		for c := uint64(0); c < n; c += rvs.ClusterSize {
			index := uint32((piece*pieces.PieceSize + c) / rvs.ClusterSize)
			if err := h.data(d.ID, index, buf[c:min(c+rvs.ClusterSize, n)]); err != nil {
				return nil, err
			}
		}
		res.Bytes += n
		if piece++; piece == d.Pieces {
			if sum, err := pt.Sum(); err != nil || sum != d.SHA256PT {
				return nil, integrity("pieces tar: %s sha256pt %s, manifest %s (%v)", d.Name, sum, d.SHA256PT, err)
			}
			if dev, piece = dev+1, 0; dev < len(devs) {
				pt = pieces.NewPT(devs[dev].Size)
			}
		}
	}
	if m == nil {
		return nil, integrity("pieces tar: no manifest: the input is not the tar of a pieces snapshot")
	}
	if next < len(want) {
		return nil, integrity("pieces tar ends after %d of %d files (truncated)", next, len(want))
	}
	if w := o.Pieces; w != nil {
		if err := samePieces(m, w); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// samePieces compares the snapshot's manifest with the copy the control code kept: the same
// disks (id, name, size) with the same sha256pt.
func samePieces(got, want *pieces.Manifest) error {
	var probs []string
	if len(got.Devices) != len(want.Devices) {
		probs = append(probs, fmt.Sprintf("%d disks in the snapshot, %d in the manifest", len(got.Devices), len(want.Devices)))
	}
	for i := range min(len(got.Devices), len(want.Devices)) {
		g, w := got.Devices[i], want.Devices[i]
		if g.ID != w.ID || g.Name != w.Name || g.Size != w.Size || g.SHA256PT != w.SHA256PT {
			probs = append(probs, fmt.Sprintf("disk %d is %s, %d bytes, sha256pt %s in the snapshot; %s, %d bytes, %s in the manifest",
				g.ID, g.Name, g.Size, g.SHA256PT, w.Name, w.Size, w.SHA256PT))
		}
	}
	if len(probs) > 0 {
		return fmt.Errorf("%w: %s", pieces.ErrManifest, strings.Join(probs, "; "))
	}
	return nil
}

// VerifyPieces reads the tar of a pieces snapshot and checks every piece and every disk's
// sha256pt against its manifest, and the optional o.Pieces.
func VerifyPieces(in io.Reader, o Options) (*Result, error) {
	return walkPieces(in, nopHandler{}, o, false)
}

// ToVMAPieces is ToVMA for the tar of a pieces snapshot.
func ToVMAPieces(in io.Reader, out io.Writer, o Options) (*Result, error) {
	return toVMA(out, func(h handler) (*Result, error) { return walkPieces(in, h, o, true) })
}

// ToRawPieces is ToRaw for the tar of a pieces snapshot.
func ToRawPieces(in io.Reader, dir string, o Options) (*Result, error) {
	return toRaw(dir, func(h handler) (*Result, error) { return walkPieces(in, h, o, false) })
}
