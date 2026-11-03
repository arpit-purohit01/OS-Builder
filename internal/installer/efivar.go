package installer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"foundry.dev/foundry/internal/gpt"
)

const globalGUID = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

// LoadOption encodes a UEFI EFI_LOAD_OPTION for a file on a GPT partition.
func LoadOption(desc string, part gpt.Partition, path string) []byte {
	var dp bytes.Buffer
	// Hard drive media device path (type 4, subtype 1, 42 bytes).
	dp.Write([]byte{4, 1, 42, 0})
	binary.Write(&dp, binary.LittleEndian, uint32(part.Index))
	binary.Write(&dp, binary.LittleEndian, part.FirstLBA)
	binary.Write(&dp, binary.LittleEndian, part.LastLBA-part.FirstLBA+1)
	dp.Write(part.UUID[:]) // GPT GUIDs are stored in on-disk (mixed-endian) order
	dp.Write([]byte{2, 2}) // MBR type GPT, signature type GUID
	// File path (type 4, subtype 4).
	p := utf16.Encode([]rune(strings.ReplaceAll(path, "/", `\`) + "\x00"))
	binary.Write(&dp, binary.LittleEndian, [2]byte{4, 4})
	binary.Write(&dp, binary.LittleEndian, uint16(4+2*len(p)))
	binary.Write(&dp, binary.LittleEndian, p)
	dp.Write([]byte{0x7f, 0xff, 4, 0}) // end of device path

	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(1)) // LOAD_OPTION_ACTIVE
	binary.Write(&b, binary.LittleEndian, uint16(dp.Len()))
	binary.Write(&b, binary.LittleEndian, utf16.Encode([]rune(desc+"\x00")))
	b.Write(dp.Bytes())
	return b.Bytes()
}

const attrs = 0x7 // NON_VOLATILE | BOOTSERVICE_ACCESS | RUNTIME_ACCESS

func readVar(dir, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, name+"-"+globalGUID))
	if err != nil || len(b) < 4 {
		return nil, err
	}
	return b[4:], nil
}

// writeVar writes an efivarfs variable, clearing the immutable flag the
// kernel sets on existing variables.
func writeVar(dir, name string, data []byte) error {
	p := filepath.Join(dir, name+"-"+globalGUID)
	if f, err := os.Open(p); err == nil {
		var flags int32
		syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x80086601, uintptr(unsafe.Pointer(&flags))) // FS_IOC_GETFLAGS
		flags &^= 0x10                                                                          // FS_IMMUTABLE_FL
		syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x40086602, uintptr(unsafe.Pointer(&flags))) // FS_IOC_SETFLAGS
		f.Close()
	}
	buf := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(buf, attrs)
	copy(buf[4:], data)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	// efivarfs needs the whole variable in one write.
	_, err = f.Write(buf)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// AddBootEntry creates (or replaces an identical) Boot#### entry and puts it
// first in BootOrder when first is set, otherwise last.
func AddBootEntry(efivars, desc string, part gpt.Partition, loader string, first bool) (string, error) {
	opt := LoadOption(desc, part, loader)
	ents, _ := filepath.Glob(filepath.Join(efivars, "Boot[0-9A-F][0-9A-F][0-9A-F][0-9A-F]-"+globalGUID))
	used := map[uint16]bool{}
	var num uint16
	found := false
	for _, e := range ents {
		var n uint16
		fmt.Sscanf(strings.TrimPrefix(filepath.Base(e), "Boot"), "%04X", &n)
		used[n] = true
		if b, err := readVar(efivars, fmt.Sprintf("Boot%04X", n)); err == nil && bytes.Equal(b, opt) {
			num, found = n, true
		}
	}
	if !found {
		for num = 0; used[num]; num++ {
		}
		if err := writeVar(efivars, fmt.Sprintf("Boot%04X", num), opt); err != nil {
			return "", err
		}
	}
	order, _ := readVar(efivars, "BootOrder")
	var list []uint16
	for i := 0; i+1 < len(order); i += 2 {
		if n := binary.LittleEndian.Uint16(order[i:]); n != num {
			list = append(list, n)
		}
	}
	if first {
		list = append([]uint16{num}, list...)
	} else {
		list = append(list, num)
	}
	var ob bytes.Buffer
	binary.Write(&ob, binary.LittleEndian, list)
	return fmt.Sprintf("Boot%04X", num), writeVar(efivars, "BootOrder", ob.Bytes())
}
