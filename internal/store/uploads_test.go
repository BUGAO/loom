package store

import (
	"bytes"
	"testing"
)

func TestUploadsRoundtrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte{0x89, 'P', 'N', 'G', 0, 1, 2}
	if err := s.SaveUpload("run_x", "img-1.png", data); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.ReadUpload("run_x", "img-1.png")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("upload bytes did not roundtrip")
	}
}

// Upload names come from the engine, but run ids and names both pass through
// HTTP paths on the way back out — traversal must die at the store boundary.
func TestUploadsRefuseTraversal(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../../etc/passwd", "a/b.png", ""} {
		if err := s.SaveUpload("run_x", name, []byte("x")); err == nil {
			t.Fatalf("save with name %q should be refused", name)
		}
		if _, err := s.ReadUpload("run_x", name); err == nil {
			t.Fatalf("read with name %q should be refused", name)
		}
	}
	if _, err := s.ReadUpload("../run_x", "img.png"); err == nil {
		t.Fatal("traversal in run id should be refused")
	}
}
