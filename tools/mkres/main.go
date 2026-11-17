// mkres writes the Windows resources of modbus-slave.exe as a COFF object (.syso) that go build
// links in: the icon (group 1), the version info (napilab, the version) and the manifest
// (common controls 6, DPI aware, asInvoker). No tools outside Go.
//
//	go run ./tools/mkres -ico assets/napi.ico -version 2.0.1 -o cmd/modbus-slave/rsrc_windows_amd64.syso
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16
	rtManifest  = 24
	langNeutral = 0x0409
)

type res struct {
	typ, id uint32
	data    []byte
}

func main() {
	ico := flag.String("ico", "assets/napi.ico", "the icon")
	ver := flag.String("version", "0.0.0", "x.y.z")
	out := flag.String("o", "cmd/modbus-slave/rsrc_windows_amd64.syso", "the object")
	flag.Parse()

	b, err := os.ReadFile(*ico)
	if err != nil {
		log.Fatal(err)
	}
	rs, err := icon(b)
	if err != nil {
		log.Fatal(err)
	}
	v := versionParts(*ver)
	rs = append(rs, res{rtVersion, 1, versionInfo(v, *ver)}, res{rtManifest, 1, []byte(manifest(v))})
	if err := os.WriteFile(*out, coff(rs), 0o644); err != nil {
		log.Fatal(err)
	}
}

// .ico -> RT_ICON 1..n and RT_GROUP_ICON 1 (the same directory with resource ids for offsets).
func icon(b []byte) ([]res, error) {
	if len(b) < 6 || binary.LittleEndian.Uint16(b[2:]) != 1 {
		return nil, fmt.Errorf("not an .ico")
	}
	n := int(binary.LittleEndian.Uint16(b[4:]))
	var out []res
	var grp bytes.Buffer
	grp.Write(b[:6])
	for i := 0; i < n; i++ {
		e := b[6+16*i : 6+16*(i+1)]
		size := binary.LittleEndian.Uint32(e[8:])
		off := binary.LittleEndian.Uint32(e[12:])
		if int(off+size) > len(b) {
			return nil, fmt.Errorf("broken .ico entry %d", i)
		}
		out = append(out, res{rtIcon, uint32(i + 1), b[off : off+size]})
		grp.Write(e[:12])
		binary.Write(&grp, binary.LittleEndian, uint16(i+1))
	}
	return append(out, res{rtGroupIcon, 1, grp.Bytes()}), nil
}

func versionParts(s string) [4]uint16 {
	var v [4]uint16
	for i, p := range strings.SplitN(strings.SplitN(s, "~", 2)[0], ".", 4) {
		n, _ := strconv.Atoi(p)
		v[i] = uint16(n)
	}
	return v
}

func manifest(v [4]uint16) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity type="win32" name="napilab.modbus-slave" version="` + fmt.Sprintf("%d.%d.%d.%d", v[0], v[1], v[2], v[3]) + `" processorArchitecture="amd64"/>
  <description>NAPI discover</description>
  <dependency>
    <dependentAssembly>
      <assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/>
    </dependentAssembly>
  </dependency>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security><requestedPrivileges><requestedExecutionLevel level="asInvoker" uiAccess="false"/></requestedPrivileges></security>
  </trustInfo>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings><dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true</dpiAware></windowsSettings>
  </application>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application><supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/></application>
  </compatibility>
</assembly>
`
}

// VS_VERSIONINFO: a tree of {wLength, wValueLength, wType, key, value, children}, 4-byte aligned.
func versionInfo(v [4]uint16, ver string) []byte {
	ms := uint32(v[0])<<16 | uint32(v[1])
	ls := uint32(v[2])<<16 | uint32(v[3])
	var fixed bytes.Buffer
	for _, x := range []uint32{0xFEEF04BD, 0x00010000, ms, ls, ms, ls, 0x3F, 0, 0x40004, 1, 0, 0, 0} {
		binary.Write(&fixed, binary.LittleEndian, x)
	}
	var strs [][]byte
	for _, kv := range [][2]string{
		{"CompanyName", "napilab"},
		{"FileDescription", "modbus-slave: эмулятор датчиков Modbus RTU/TCP"},
		{"FileVersion", ver},
		{"InternalName", "modbus-slave"},
		{"LegalCopyright", "napilab, lab240"},
		{"OriginalFilename", "modbus-slave.exe"},
		{"ProductName", "modbus-slave"},
		{"ProductVersion", ver},
	} {
		val := u16z(kv[1])
		strs = append(strs, node(kv[0], 1, val, uint16(len(val)/2)))
	}
	table := node("040904B0", 1, nil, 0, strs...)
	sfi := node("StringFileInfo", 1, nil, 0, table)
	tr := []byte{0x09, 0x04, 0xB0, 0x04} // 0x0409, 1200 (UTF-16)
	vfi := node("VarFileInfo", 1, nil, 0, node("Translation", 0, tr, 4))
	return node("VS_VERSION_INFO", 0, fixed.Bytes(), uint16(fixed.Len()), sfi, vfi)
}

func node(key string, typ uint16, value []byte, valueLen uint16, children ...[]byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, valueLen, typ})
	b.Write(u16z(key))
	pad4(&b)
	b.Write(value)
	for _, c := range children {
		pad4(&b)
		b.Write(c)
	}
	out := b.Bytes()
	binary.LittleEndian.PutUint16(out, uint16(len(out)))
	return out
}

func u16z(s string) []byte {
	w := utf16.Encode([]rune(s + "\x00"))
	b := make([]byte, 2*len(w))
	for i, x := range w {
		binary.LittleEndian.PutUint16(b[2*i:], x)
	}
	return b
}

func pad4(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

// The COFF object: one .rsrc section with the resource directory (type -> id -> language), the
// data entries, the data, and an ADDR32NB relocation for each data entry; one section symbol.
func coff(rs []res) []byte {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].typ != rs[j].typ {
			return rs[i].typ < rs[j].typ
		}
		return rs[i].id < rs[j].id
	})
	var types []uint32
	byType := map[uint32][]res{}
	for _, r := range rs {
		if byType[r.typ] == nil {
			types = append(types, r.typ)
		}
		byType[r.typ] = append(byType[r.typ], r)
	}
	dir := func(n int) int { return 16 + 8*n }
	// Layout: root, the type dirs, the id dirs (one language each), the data entries, the data.
	off := dir(len(types))
	typeOff := map[uint32]int{}
	for _, t := range types {
		typeOff[t] = off
		off += dir(len(byType[t]))
	}
	langOff := make([]int, len(rs))
	for i := range rs {
		langOff[i] = off
		off += dir(1)
	}
	entryOff := make([]int, len(rs))
	for i := range rs {
		entryOff[i] = off
		off += 16
	}
	dataOff := make([]int, len(rs))
	for i := range rs {
		off = (off + 7) &^ 7
		dataOff[i] = off
		off += len(rs[i].data)
	}
	sec := make([]byte, (off+7)&^7)
	le := binary.LittleEndian
	putDir := func(at, n int) { le.PutUint16(sec[at+14:], uint16(n)) } // id entries only
	putEnt := func(at, i int, id, to uint32) { le.PutUint32(sec[at+16+8*i:], id); le.PutUint32(sec[at+16+8*i+4:], to) }

	putDir(0, len(types))
	k := 0
	for ti, t := range types {
		putEnt(0, ti, t, 0x80000000|uint32(typeOff[t]))
		putDir(typeOff[t], len(byType[t]))
		for ii, r := range byType[t] {
			putEnt(typeOff[t], ii, r.id, 0x80000000|uint32(langOff[k]))
			putDir(langOff[k], 1)
			putEnt(langOff[k], 0, langNeutral, uint32(entryOff[k]))
			le.PutUint32(sec[entryOff[k]:], uint32(dataOff[k])) // + the section RVA (relocation)
			le.PutUint32(sec[entryOff[k]+4:], uint32(len(r.data)))
			copy(sec[dataOff[k]:], r.data)
			k++
		}
	}

	var f bytes.Buffer
	const hdr, shdr = 20, 40
	relocAt := hdr + shdr + len(sec)
	symAt := relocAt + 10*len(rs)
	binary.Write(&f, le, struct {
		Machine, Sections        uint16
		Time, SymPtr, Syms       uint32
		OptSize, Characteristics uint16
	}{0x8664, 1, 0, uint32(symAt), 1, 0, 0})
	binary.Write(&f, le, struct {
		Name                                  [8]byte
		VSize, VAddr, RawSize, RawPtr, RelPtr uint32
		LinePtr                               uint32
		Relocs, Lines                         uint16
		Characteristics                       uint32
	}{[8]byte{'.', 'r', 's', 'r', 'c'}, 0, 0, uint32(len(sec)), hdr + shdr, uint32(relocAt), 0, uint16(len(rs)), 0, 0x40000040})
	f.Write(sec)
	for i := range rs {
		binary.Write(&f, le, uint32(entryOff[i])) // where
		binary.Write(&f, le, uint32(0))           // symbol 0: .rsrc
		binary.Write(&f, le, uint16(3))           // IMAGE_REL_AMD64_ADDR32NB
	}
	binary.Write(&f, le, struct {
		Name    [8]byte
		Value   uint32
		Section int16
		Type    uint16
		Class   uint8
		Aux     uint8
	}{[8]byte{'.', 'r', 's', 'r', 'c'}, 0, 1, 0, 3, 0})
	binary.Write(&f, le, uint32(4)) // the empty string table
	return f.Bytes()
}
