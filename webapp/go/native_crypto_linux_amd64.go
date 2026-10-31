package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"log"
	"os"
	"runtime"
	"strconv"
	"unsafe"

	"github.com/ebitengine/purego"
)

func init() {
	path := os.Getenv("NATIVE_CRYPTO_LIB")
	if path == "" {
		return
	}
	library, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		log.Fatalf("load native crypto: %v", err)
	}
	var batch func(unsafe.Pointer, uint32, unsafe.Pointer) int32
	var architecture func() int32
	purego.RegisterLibFunc(&batch, library, "isureal_sign_batch")
	purego.RegisterLibFunc(&architecture, library, "isureal_crypto_arch")
	nativeSignatureBatch = func(jobs []signatureJob) ([]string, bool) {
		if len(jobs) == 0 {
			return []string{}, true
		}
		if len(jobs) > 256 {
			return nil, false
		}
		input := make([]byte, len(jobs)*164)
		for i, j := range jobs {
			key, err := hex.DecodeString(j.Secret)
			message := strconv.FormatInt(j.Exp, 10) + ":" + j.ID
			if err != nil || len(key) != 32 || len(message) > 128 {
				return nil, false
			}
			record := input[i*164:]
			copy(record, key)
			binary.LittleEndian.PutUint32(record[32:], uint32(len(message)))
			copy(record[36:], message)
		}
		raw := make([]byte, len(jobs)*32)
		result := batch(unsafe.Pointer(&input[0]), uint32(len(jobs)), unsafe.Pointer(&raw[0]))
		runtime.KeepAlive(input)
		runtime.KeepAlive(raw)
		if result != 0 {
			return nil, false
		}
		out := make([]string, len(jobs))
		for i := range out {
			out[i] = base64.RawURLEncoding.EncodeToString(raw[i*32 : (i+1)*32])
		}
		return out, true
	}
	// Check the selected host instruction path before accepting any requests.
	jobs := make([]signatureJob, 17)
	for i := range jobs {
		jobs[i] = signatureJob{ID: "startup-" + strconv.Itoa(i), Secret: secretHash("key-" + strconv.Itoa(i)), Exp: 1800000000 + int64(i)}
	}
	got, ok := nativeSignatureBatch(jobs)
	if !ok {
		log.Fatal("native crypto startup check failed")
	}
	for i, j := range jobs {
		if got[i] != computeImageSignature(j.ID, j.Secret, j.Exp) {
			log.Fatal("native crypto startup result mismatch")
		}
	}
	nativeCryptoArch.Store(int64(architecture()))
	log.Printf("native multi-buffer HMAC ready: architecture=%d", nativeCryptoArch.Load())
}
