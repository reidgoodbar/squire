package proofcache

import (
	"encoding/binary"
	"testing"
)

func TestRepoSearchLineBloomsContainEveryIndexedNGram(t *testing.T) {
	line := []byte("Alpha caf\xc3\xa9")
	bloom, foldedBloom, foldUnsafe := repoSearchLineBlooms(line)
	if foldUnsafe {
		t.Fatal("non-ASCII bytes without an ASCII case-fold mapping were marked unsafe")
	}
	for width := 1; width <= 3; width++ {
		for start := 0; start+width <= len(line); start++ {
			window := append([]byte(nil), line[start:start+width]...)
			bits := repoSearchBloomBits(window)
			if bloom&bits != bits {
				t.Fatalf("exact bloom rejected %q", window)
			}
			for i, value := range window {
				if value >= 'A' && value <= 'Z' {
					window[i] = value + ('a' - 'A')
				}
			}
			foldedBits := repoSearchBloomBits(window)
			if foldedBloom&foldedBits != foldedBits {
				t.Fatalf("folded bloom rejected %q", window)
			}
		}
	}
}

func TestRepoSearchLineBloomsMarkASCIICaseFoldHazards(t *testing.T) {
	for _, line := range [][]byte{
		[]byte("kelvin \u212A"),
		[]byte("long-s \u017F"),
		{0xff},
	} {
		if _, _, unsafe := repoSearchLineBlooms(line); !unsafe {
			t.Fatalf("line %q was not marked case-fold unsafe", line)
		}
	}
}

func TestEncodeRepoSearchCorpusIncludesVersionedLineIndex(t *testing.T) {
	files := []repoSearchCorpusFile{
		{path: "alpha.txt", content: []byte("Alpha beta\nunicode\u00a0space\nlast")},
		{path: "empty.txt", content: nil},
	}
	for i := range files {
		files[i].lines, files[i].foldUnsafe = indexRepoSearchCorpusLines(files[i].content)
	}
	frame, ok := encodeRepoSearchCorpus(files, []uint32{0, 1}, []uint32{0, 1})
	if !ok {
		t.Fatal("encodeRepoSearchCorpus failed")
	}
	if got := binary.LittleEndian.Uint32(frame[8:12]); got != repoSearchCorpusVersion {
		t.Fatalf("version = %d, want %d", got, repoSearchCorpusVersion)
	}
	lineRecordsOffset := int(binary.LittleEndian.Uint32(frame[44:48]))
	payloadOffset := int(binary.LittleEndian.Uint32(frame[36:40]))
	if lineRecordsOffset <= repoSearchCorpusHeaderBytes || payloadOffset <= lineRecordsOffset {
		t.Fatalf("invalid offsets: lines=%d payload=%d", lineRecordsOffset, payloadOffset)
	}
	wantLines := [][]byte{[]byte("Alpha beta"), []byte("unicode\u00a0space"), []byte("last")}
	firstRecord := frame[repoSearchCorpusHeaderBytes : repoSearchCorpusHeaderBytes+repoSearchCorpusRecordBytes]
	lineStart := binary.LittleEndian.Uint32(firstRecord[16:20])
	encodedLineCount := binary.LittleEndian.Uint32(firstRecord[20:24])
	if lineStart != 0 || encodedLineCount&repoSearchCorpusFoldUnsafeFlag != 0 ||
		encodedLineCount&^repoSearchCorpusFoldUnsafeFlag != uint32(len(wantLines)) {
		t.Fatalf("first file line metadata = start %d count %#x", lineStart, encodedLineCount)
	}
	contentOffset := int(binary.LittleEndian.Uint32(firstRecord[8:12]))
	for i, want := range wantLines {
		record := frame[lineRecordsOffset+i*repoSearchCorpusLineBytes : lineRecordsOffset+(i+1)*repoSearchCorpusLineBytes]
		offset := int(binary.LittleEndian.Uint32(record[0:4]))
		length := int(binary.LittleEndian.Uint32(record[4:8]))
		if got := frame[contentOffset+offset : contentOffset+offset+length]; string(got) != string(want) {
			t.Fatalf("line %d = %q, want %q", i+1, got, want)
		}
		if binary.LittleEndian.Uint64(record[8:16]) == 0 || binary.LittleEndian.Uint64(record[16:24]) == 0 {
			t.Fatalf("line %d has an empty bloom", i+1)
		}
	}
	secondRecord := frame[repoSearchCorpusHeaderBytes+repoSearchCorpusRecordBytes : repoSearchCorpusHeaderBytes+2*repoSearchCorpusRecordBytes]
	if got := binary.LittleEndian.Uint32(secondRecord[16:20]); got != uint32(len(wantLines)) {
		t.Fatalf("empty file line start = %d, want %d", got, len(wantLines))
	}
	if got := binary.LittleEndian.Uint32(secondRecord[20:24]); got != 0 {
		t.Fatalf("empty file encoded line count = %#x, want 0", got)
	}
	if got := binary.LittleEndian.Uint32(frame[40:44]); got != uint32(len(frame)) {
		t.Fatalf("encoded total = %d, want %d", got, len(frame))
	}
}
