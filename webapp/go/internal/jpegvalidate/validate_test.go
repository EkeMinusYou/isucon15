package jpegvalidate

import (
	"bytes"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMatchesStandardDecoder(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(runtime.GOROOT(), "src/image/testdata/*.jpeg"))
	if len(files) == 0 {
		t.Fatal("missing fixture corpus")
	}
	rng := rand.New(rand.NewSource(41))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		check := func(data []byte) {
			_, want := jpeg.Decode(bytes.NewReader(data))
			got := Validate(bytes.NewReader(data))
			if (want == nil) != (got == nil) {
				t.Fatalf("%s: standard=%v validator=%v", f, want, got)
			}
		}
		check(b)
		for i := 0; i < 40; i++ {
			check(b[:rng.Intn(len(b))])
			c := append([]byte(nil), b...)
			c[rng.Intn(len(c))] ^= byte(rng.Intn(255) + 1)
			check(c)
		}
	}
}
func BenchmarkValidate(b *testing.B) {
	data, err := os.ReadFile(filepath.Join(runtime.GOROOT(), "src/image/testdata/video-001.jpeg"))
	if err != nil {
		b.Fatal(err)
	}
	for _, test := range []struct {
		name string
		f    func([]byte)
	}{
		{"standard", func(p []byte) { jpeg.Decode(bytes.NewReader(p)) }},
		{"entropy", func(p []byte) { Validate(bytes.NewReader(p)) }},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				test.f(data)
			}
		})
	}
}
