package specindex

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// FTR.HMR.SPEC-0004 R7: the contract files have their types, open in the viewer and give it their text.
func TestContractFiles(t *testing.T) {
	for name, want := range map[string]string{
		"tech/booking.openapi.yaml": MimeOpenAPIYAML, "booking.openapi.yml": MimeOpenAPIYAML,
		"booking.openapi.json": MimeOpenAPIJSON, "booking.proto": MimeProto, "booking.json": "application/json",
	} {
		if got := MimeOf(name); got != want {
			t.Errorf("%s: %s", name, got)
		}
	}
	if !Previewable(MimeProto) || contractMime("booking.proto", "application/octet-stream") != MimeProto {
		t.Error("a contract stored before the types")
	}
	w := httptest.NewRecorder()
	if err := ServeFile(w, "booking.proto", MimeProto, []byte("syntax = \"proto3\";"), "preview", 1<<20, true); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["text"] != `syntax = "proto3";` || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("preview %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	_ = ServeFile(w, "booking.proto", MimeProto, []byte("x"), "", 1<<20, true)
	if w.Header().Get("Content-Disposition") == "" {
		t.Error("a download without a mode")
	}
}
