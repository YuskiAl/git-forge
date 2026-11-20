// Self-test harness (temporary scaffolding): run with
// FORGE_SELFTEST=pack to exercise pack round-trips and print diagnostics.
package main

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"os"
)

func runSelfTest(kind string) int {
	switch kind {
	case "pack":
		return selfTestPack()
	case "packfile":
		return selfTestPackFile(os.Getenv("FORGE_PACKFILE"))
	default:
		fmt.Fprintf(os.Stderr, "unknown selftest %q\n", kind)
		return 2
	}
}

func selfTestPack() int {
	blob := bytes.Repeat([]byte("hello world, this is a test blob. "), 40) // 1320 bytes
	blobOID := hashObject(objBlob, blob)
	tree := buildTree([]TreeEntry{{Mode: "100644", Name: "file.txt", OID: blobOID}})
	treeOID := hashObject(objTree, tree)
	commit := buildCommitText(treeOID, nil, "test commit\n", 1700000000)
	commitOID := hashObject(objCommit, commit)
	objs := []*GitObject{
		{Type: objBlob, Data: blob},
		{Type: objTree, Data: tree},
		{Type: objCommit, Data: commit},
	}
	pack1 := writePack(objs)
	got1, err := readPack(pack1, nil)
	if err != nil {
		fmt.Println("readPack pack1:", err)
		return 1
	}
	fmt.Println("pack1 len", len(pack1), "objects", len(got1))
	for _, o := range objs {
		oid := hashObject(o.Type, o.Data)
		g := got1[oid]
		fmt.Printf("  %s type=%s rawzlib=%d data=%d match=%v\n", oid[:8], o.Type, len(g.RawZlib), len(g.Data), bytes.Equal(g.Data, o.Data))
	}
	var reemit []*GitObject
	for _, oid := range []string{commitOID, treeOID, blobOID} {
		reemit = append(reemit, got1[oid])
	}
	pack2 := writePack(reemit)
	fmt.Println("pack2 len", len(pack2))
	got2, err := readPack(pack2, nil)
	if err != nil {
		fmt.Println("readPack pack2:", err)
		return 1
	}
	fmt.Println("pack2 objects", len(got2))
	fail := 0
	for _, o := range objs {
		oid := hashObject(o.Type, o.Data)
		g := got2[oid]
		if g == nil || !bytes.Equal(g.Data, o.Data) {
			fmt.Printf("  MISMATCH %s\n", oid)
			fail++
		}
	}
	// Also verify with a strict sequential parse mimicking the kit client:
	// each entry's zlib stream must inflate to exactly the declared size
	// and consume exactly its own bytes.
	if err := verifyPackSequential(pack2); err != nil {
		fmt.Println("verifyPackSequential pack2:", err)
		fail++
	}
	if err := verifyPackSequential(pack1); err != nil {
		fmt.Println("verifyPackSequential pack1:", err)
		fail++
	}
	if fail > 0 {
		return 1
	}
	fmt.Println("PACK SELFTEST OK")
	return 0
}

func selfTestPackFile(path string) int {
	pack, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("read packfile:", err)
		return 2
	}
	fmt.Println("packfile", path, "len", len(pack))
	if err := verifyPackSequential(pack); err != nil {
		fmt.Println("verifyPackSequential input:", err)
		return 1
	}
	fmt.Println("input sequential verify OK")
	var extern map[string]*GitObject
	if basePath := os.Getenv("FORGE_PACKFILE_BASE"); basePath != "" {
		basePack, err := os.ReadFile(basePath)
		if err != nil {
			fmt.Println("read base packfile:", err)
			return 2
		}
		extern, err = readPack(basePack, nil)
		if err != nil {
			fmt.Println("readPack base:", err)
			return 1
		}
		fmt.Println("base objects", len(extern))
	}
	got, err := readPack(pack, extern)
	if err != nil {
		fmt.Println("readPack input:", err)
		return 1
	}
	fmt.Println("readPack objects", len(got))
	for oid, o := range got {
		fmt.Printf("  %s type=%s rawzlib=%d data=%d\n", oid[:8], o.Type, len(o.RawZlib), len(o.Data))
	}
	var objs []*GitObject
	for _, o := range got {
		objs = append(objs, o)
	}
	pack2 := writePack(objs)
	fmt.Println("re-emitted len", len(pack2))
	if err := verifyPackSequential(pack2); err != nil {
		fmt.Println("verifyPackSequential re-emitted:", err)
		return 1
	}
	fmt.Println("re-emitted sequential verify OK")
	got2, err := readPack(pack2, nil)
	if err != nil {
		fmt.Println("readPack re-emitted:", err)
		return 1
	}
	fail := 0
	for oid, o := range got {
		g := got2[oid]
		if g == nil || !bytes.Equal(g.Data, o.Data) || g.Type != o.Type {
			fmt.Printf("  MISMATCH %s\n", oid)
			fail++
		}
	}
	if fail > 0 {
		return 1
	}
	fmt.Println("PACKFILE SELFTEST OK")
	return 0
}

// verifyPackSequential parses a pack the way the kit's git-client does:
// header, per-entry varint, then one zlib inflate whose consumed byte
// count advances the offset.
func verifyPackSequential(pack []byte) error {
	if len(pack) < 32 || string(pack[0:4]) != "PACK" {
		return fmt.Errorf("bad header")
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	offset := 12
	for i := 0; i < count; i++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(pack, offset)
		if err != nil {
			return err
		}
		offset = next
		tc := int((first >> 4) & 0x07)
		if tc == 6 {
			_, nxt, err := readOfsBase(pack, offset)
			if err != nil {
				return err
			}
			offset = nxt
		} else if tc == 7 {
			offset += 20
		}
		br := bytes.NewReader(pack[offset:trailerStart])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return fmt.Errorf("entry %d at %d: zlib open: %v", i, entryOffset, err)
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return fmt.Errorf("entry %d at %d: inflate: %v", i, entryOffset, err)
		}
		if len(data) != size {
			return fmt.Errorf("entry %d at %d declares %d but inflated to %d", i, entryOffset, size, len(data))
		}
		consumed := (trailerStart - offset) - br.Len()
		offset += consumed
	}
	if offset != trailerStart {
		return fmt.Errorf("entries end at %d, trailer at %d", offset, trailerStart)
	}
	return nil
}
