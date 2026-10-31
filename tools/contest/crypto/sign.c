#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <intel-ipsec-mb.h>

/* Wire record: 32-byte key, little-endian message length, 128-byte message. */
int isureal_sign_batch(const uint8_t *records, uint32_t count, uint8_t *result) {
    if (!count || count > 256) return -1;
    IMB_MGR *mgr = alloc_mb_mgr(0);
    if (!mgr) return -2;
    init_mb_mgr_auto(mgr, NULL);
    uint8_t *pads = aligned_alloc(64, (size_t)count * 64);
    uint8_t *a = aligned_alloc(64, ((size_t)count * 32 + 63) & ~(size_t)63);
    uint8_t *b = aligned_alloc(64, ((size_t)count * 32 + 63) & ~(size_t)63);
    if (!pads || !a || !b) { free(pads); free(a); free(b); free_mb_mgr(mgr); return -3; }
    int rc = 0;
    for (uint32_t i = 0; i < count; i++) {
        uint8_t block[64];
        const uint8_t *key = records + i * 164;
        memset(block, 0x36, 64);
        for (int k = 0; k < 32; k++) block[k] ^= key[k];
        IMB_SHA256_ONE_BLOCK(mgr, block, pads + i * 64);
        memset(block, 0x5c, 64);
        for (int k = 0; k < 32; k++) block[k] ^= key[k];
        IMB_SHA256_ONE_BLOCK(mgr, block, pads + i * 64 + 32);
    }
    for (int round = 0; round < 1000 && !rc; round++) {
        uint8_t *dst = (round & 1) ? b : a;
        const uint8_t *previous = (round & 1) ? a : b;
        for (uint32_t i = 0; i < count; i++) {
            const uint8_t *record = records + i * 164;
            uint32_t length = 32;
            if (!round) { memcpy(&length, record + 32, 4); if (length > 128) { rc = -4; break; } }
            IMB_JOB *job = IMB_GET_NEXT_JOB(mgr);
            memset(job, 0, sizeof(*job));
            job->cipher_mode = IMB_CIPHER_NULL;
            job->cipher_direction = IMB_DIR_ENCRYPT;
            job->chain_order = IMB_ORDER_HASH_CIPHER;
            job->hash_alg = IMB_AUTH_HMAC_SHA_256;
            job->src = round ? previous + i * 32 : record + 36;
            job->msg_len_to_hash_in_bytes = length;
            job->hash_start_src_offset_in_bytes = 0;
            job->auth_tag_output = dst + i * 32;
            job->auth_tag_output_len_in_bytes = 32;
            job->u.HMAC._hashed_auth_key_xor_ipad = pads + i * 64;
            job->u.HMAC._hashed_auth_key_xor_opad = pads + i * 64 + 32;
            job = IMB_SUBMIT_JOB(mgr);
            if (job && job->status != IMB_STATUS_COMPLETED) rc = -5;
        }
        IMB_JOB *job;
        while ((job = IMB_FLUSH_JOB(mgr))) if (job->status != IMB_STATUS_COMPLETED) rc = -6;
    }
    if (!rc) memcpy(result, b, (size_t)count * 32);
    free(pads); free(a); free(b); free_mb_mgr(mgr);
    return rc;
}

int isureal_crypto_arch(void) {
    IMB_MGR *mgr = alloc_mb_mgr(0);
    if (!mgr) return 0;
    IMB_ARCH arch = IMB_ARCH_NONE;
    init_mb_mgr_auto(mgr, &arch);
    free_mb_mgr(mgr);
    return (int)arch;
}
