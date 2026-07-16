package proofcache

import (
	"bytes"
	"testing"
)

func TestSedPrintSelectionPreservesNativeOverlapSemantics(t *testing.T) {
	selection, ok := parseSedPrintSelection("1,3p;2,4p;2p")
	if !ok {
		t.Fatal("valid selection rejected")
	}
	content := []byte("one\ntwo\nthree\nfour\nfive\n")
	want := []byte("one\ntwo\ntwo\ntwo\nthree\nthree\nfour\n")
	for _, starts := range [][]int{nil, lineStartsForContent(content)} {
		got, ok := sedPrintSelectionBytesIndexed(content, starts, selection, maxFileInspectionOutputBytes)
		if !ok {
			t.Fatal("selection evaluation failed")
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("selection output = %q, want %q", got, want)
		}
	}
}

func TestSedPrintSelectionBounds(t *testing.T) {
	valid := []string{
		"1p",
		"1,501p",
		"1,3p;8p;10,12p",
		"1p;1p",
	}
	for _, expr := range valid {
		if _, ok := parseSedPrintSelection(expr); !ok {
			t.Errorf("valid expression %q rejected", expr)
		}
	}

	invalid := []string{
		"",
		"0p",
		"2,1p",
		"1,502p",
		"10001p",
		"1,2",
		"1,2p;",
		"1,2p;;3p",
		"1,2p; 3p",
		"1,2p;3q",
		"1,2p;3,4p;5p;6p;7p;8p;9p;10p;11p",
	}
	for _, expr := range invalid {
		if _, ok := parseSedPrintSelection(expr); ok {
			t.Errorf("invalid expression %q accepted", expr)
		}
	}
}

func TestSedPrintSelectionHonorsOutputBound(t *testing.T) {
	selection, ok := parseSedPrintSelection("1p;1p")
	if !ok {
		t.Fatal("valid selection rejected")
	}
	if _, ok := sedPrintSelectionBytesIndexed([]byte("12345\n"), nil, selection, 10); ok {
		t.Fatal("selection exceeded output bound")
	}
}
