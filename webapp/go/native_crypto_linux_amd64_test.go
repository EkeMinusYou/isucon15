package main

import (
	"strconv"
	"testing"
)

func TestNativeBatchMatchesStandardHMAC(t *testing.T) {
	if nativeSignatureBatch == nil {
		t.Skip("native crypto is not enabled")
	}
	for _, n := range []int{1, 7, 8, 16, 17, 33, 129, 256} {
		jobs := make([]signatureJob, n)
		for i := range jobs {
			jobs[i] = signatureJob{ID: "image-日本語-" + strconv.Itoa(i), Secret: secretHash("secret-" + strconv.Itoa(i)), Exp: 1800000000 + int64(i)}
		}
		got, ok := nativeSignatureBatch(jobs)
		if !ok || len(got) != n {
			t.Fatalf("batch %d failed", n)
		}
		for i, j := range jobs {
			if got[i] != computeImageSignature(j.ID, j.Secret, j.Exp) {
				t.Fatalf("batch %d index %d differs", n, i)
			}
		}
	}
}
