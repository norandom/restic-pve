package nbdtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Spec describes the disks of a fake backup job, shared by the fake NBD server and the Perl
// vzdump stand-in (testdata/vzdump-driver.pl in internal/provider), which reads the same file.
type Spec struct {
	Devices []SpecDevice `json:"devices"`
	// Config and Firewall are the guest config the stand-in hands to backup_vm.
	Config   string `json:"config,omitempty"`
	Firewall string `json:"firewall,omitempty"`
	// ExitStatus: the stand-in exits 2 when the guest or the job failed, as vzdump does.
	ExitStatus bool `json:"exit_status,omitempty"`
}

// SpecDevice is one disk.
type SpecDevice struct {
	Name   string      `json:"name"`
	Size   uint64      `json:"size"`
	Record bool        `json:"record"`           // QEMU has a bitmap for it: "use" gets "reuse"
	Bitmap string      `json:"bitmap,omitempty"` // the stand-in reports this mode whatever was asked
	Dirty  [][2]uint64 `json:"dirty"`            // offset, length
	Fault  Fault       `json:"fault,omitempty"`
}

// Pattern is the content of a fake disk: every 4 MiB piece filled with a byte pattern of its
// own, so a piece read from the wrong place is noticed. It is generated for disks up to
// 1 GiB; past that the disk reads as zeros.
func Pattern(dev string, size uint64) []byte {
	if size > 1<<30 {
		return nil
	}
	b := make([]byte, size)
	seed := byte(len(dev))
	for i := range b {
		piece := uint64(i) >> 22
		b[i] = byte(uint64(i)*31+piece*7) ^ seed
	}
	return b
}

// LoadSpec reads a spec file.
func LoadSpec(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return &s, nil
}

// Exports returns the exports of the spec, with bitmap name bitmap.
func (s *Spec) Exports(bitmap string) Exports {
	var x Exports
	for _, d := range s.Devices {
		srv := &Server{Export: d.Name, Bitmap: bitmap, Size: d.Size, Data: Pattern(d.Name, d.Size), Fault: d.Fault}
		for _, r := range d.Dirty {
			srv.Dirty = append(srv.Dirty, Range{r[0], r[1]})
		}
		x = append(x, srv)
	}
	return x
}

// Main is the standalone fake NBD server: Main(socket, specFile, bitmap) serves the spec's
// disks on a Unix socket until SIGTERM or SIGINT, or for at most an hour. Each connection is
// one session; on exit every command received is written to <socket>.log, one
// "<export> <read|disc|trim|status|type N> <offset> <length>" line each.
func Main(args []string) error {
	if len(args) != 3 {
		return errors.New("usage: fakenbd <socket> <spec.json> <bitmap name>")
	}
	spec, err := LoadSpec(args[1])
	if err != nil {
		return err
	}
	x := spec.Exports(args[2])
	l, err := net.Listen("unix", args[0])
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				x.Serve(c)
			}()
		}
	}()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sig:
	case <-time.After(time.Hour):
	}
	l.Close()
	f, err := os.Create(args[0] + ".log")
	if err != nil {
		return err
	}
	defer f.Close()
	names := map[uint16]string{0: "read", 2: "disc", 4: "trim", 7: "status"}
	for _, s := range x {
		for _, c := range s.Commands() {
			name, ok := names[c.Type]
			if !ok {
				name = fmt.Sprintf("type%d", c.Type)
			}
			fmt.Fprintf(f, "%s %s %d %d\n", s.Export, name, c.Offset, c.Length)
		}
	}
	return nil
}
